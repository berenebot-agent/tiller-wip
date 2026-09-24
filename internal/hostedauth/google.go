// Package hostedauth implements the hosted login providers. It uses only fixed
// provider endpoints and callers must supply the hosted safe outbound client.
package hostedauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	googleAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL     = "https://oauth2.googleapis.com/token"
	googleJWKSURL      = "https://www.googleapis.com/oauth2/v3/certs"
	maxProviderBody    = 1 << 20
)

var (
	ErrGoogleResponse = errors.New("hostedauth: invalid Google response")
	ErrGoogleToken    = errors.New("hostedauth: invalid Google identity token")
)

type GoogleIdentity struct {
	Subject       string
	Email         string
	EmailVerified bool
}

type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

// GoogleVerifier exchanges an authorization code and validates Google's ID
// token against Google's public signing keys.
type GoogleVerifier struct {
	client *http.Client
	mu     sync.Mutex
	keys   map[string]*rsa.PublicKey
	keyExp time.Time
}

func NewGoogleVerifier(client *http.Client) *GoogleVerifier {
	return &GoogleVerifier{client: client}
}

// AuthorizationURL builds a minimal OIDC authorization request. The app asks
// only for identity and email, and PKCE complements the state and nonce.
func AuthorizationURL(config GoogleConfig, state, nonce, verifier string, selectAccount bool) (string, error) {
	if config.ClientID == "" || config.RedirectURI == "" || state == "" || nonce == "" || verifier == "" {
		return "", ErrGoogleResponse
	}
	challenge := sha256.Sum256([]byte(verifier))
	challengeValue := base64.RawURLEncoding.EncodeToString(challenge[:])
	values := url.Values{
		"client_id":             {config.ClientID},
		"redirect_uri":          {config.RedirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {challengeValue},
		"code_challenge_method": {"S256"},
	}
	if selectAccount {
		values.Set("prompt", "select_account")
	}
	return googleAuthorizeURL + "?" + values.Encode(), nil
}

// ExchangeCode exchanges Google's one-time authorization code and verifies the
// returned ID token. OAuth access and refresh tokens are discarded.
func (v *GoogleVerifier) ExchangeCode(ctx context.Context, config GoogleConfig, code, verifier, nonce string) (GoogleIdentity, error) {
	if v == nil || v.client == nil || config.ClientID == "" || config.ClientSecret == "" || config.RedirectURI == "" || code == "" || len(code) > 4096 || verifier == "" || nonce == "" {
		return GoogleIdentity{}, ErrGoogleResponse
	}
	form := url.Values{
		"client_id":     {config.ClientID},
		"client_secret": {config.ClientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {config.RedirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return GoogleIdentity{}, ErrGoogleResponse
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return GoogleIdentity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GoogleIdentity{}, ErrGoogleResponse
	}
	var token struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderBody)).Decode(&token); err != nil || token.IDToken == "" {
		return GoogleIdentity{}, ErrGoogleResponse
	}
	return v.ValidateIDToken(ctx, token.IDToken, config.ClientID, nonce)
}

func (v *GoogleVerifier) ValidateIDToken(ctx context.Context, raw, audience, nonce string) (GoogleIdentity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || len(raw) > maxProviderBody {
		return GoogleIdentity{}, ErrGoogleToken
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(headerBytes, &header) != nil || header.Algorithm != "RS256" || header.KeyID == "" {
		return GoogleIdentity{}, ErrGoogleToken
	}
	key, err := v.key(ctx, header.KeyID, false)
	if err != nil {
		key, err = v.key(ctx, header.KeyID, true)
		if err != nil {
			return GoogleIdentity{}, ErrGoogleToken
		}
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return GoogleIdentity{}, ErrGoogleToken
	}
	signed := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return GoogleIdentity{}, ErrGoogleToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return GoogleIdentity{}, ErrGoogleToken
	}
	var claims struct {
		Issuer        string          `json:"iss"`
		Subject       string          `json:"sub"`
		Audience      json.RawMessage `json:"aud"`
		Authorized    string          `json:"azp"`
		ExpiresAt     int64           `json:"exp"`
		IssuedAt      int64           `json:"iat"`
		Nonce         string          `json:"nonce"`
		Email         string          `json:"email"`
		EmailVerified bool            `json:"email_verified"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return GoogleIdentity{}, ErrGoogleToken
	}
	if !validAudience(claims.Audience, audience) {
		return GoogleIdentity{}, ErrGoogleToken
	}
	if claims.Authorized != "" && claims.Authorized != audience {
		return GoogleIdentity{}, ErrGoogleToken
	}
	var audienceSet []string
	if json.Unmarshal(claims.Audience, &audienceSet) == nil && len(audienceSet) > 1 && claims.Authorized != audience {
		return GoogleIdentity{}, ErrGoogleToken
	}
	now := time.Now()
	if (claims.Issuer != "https://accounts.google.com" && claims.Issuer != "accounts.google.com") || claims.Subject == "" || len(claims.Subject) > 255 || claims.Nonce != nonce || !claims.EmailVerified || claims.Email == "" || claims.ExpiresAt <= now.Unix() || claims.IssuedAt > now.Add(2*time.Minute).Unix() || claims.IssuedAt < now.Add(-10*time.Minute).Unix() {
		return GoogleIdentity{}, ErrGoogleToken
	}
	return GoogleIdentity{Subject: claims.Subject, Email: strings.ToLower(strings.TrimSpace(claims.Email)), EmailVerified: claims.EmailVerified}, nil
}

func validAudience(raw json.RawMessage, want string) bool {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single == want
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, value := range many {
		if value == want {
			return true
		}
	}
	return false
}

func (v *GoogleVerifier) key(ctx context.Context, kid string, force bool) (*rsa.PublicKey, error) {
	if v == nil || v.client == nil {
		return nil, ErrGoogleResponse
	}
	v.mu.Lock()
	if !force && time.Now().Before(v.keyExp) {
		if key := v.keys[kid]; key != nil {
			v.mu.Unlock()
			return key, nil
		}
		v.mu.Unlock()
		return nil, ErrGoogleToken
	}
	v.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleJWKSURL, nil)
	if err != nil {
		return nil, ErrGoogleResponse
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ErrGoogleResponse
	}
	var jwks struct {
		Keys []struct {
			KeyType  string `json:"kty"`
			Use      string `json:"use"`
			KeyID    string `json:"kid"`
			Modulus  string `json:"n"`
			Exponent string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderBody)).Decode(&jwks); err != nil {
		return nil, ErrGoogleResponse
	}
	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, rawKey := range jwks.Keys {
		if rawKey.KeyType != "RSA" || (rawKey.Use != "" && rawKey.Use != "sig") || rawKey.KeyID == "" {
			continue
		}
		modulus, err := base64.RawURLEncoding.DecodeString(rawKey.Modulus)
		if err != nil {
			continue
		}
		exponentBytes, err := base64.RawURLEncoding.DecodeString(rawKey.Exponent)
		if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
			continue
		}
		exponent := 0
		for _, b := range exponentBytes {
			exponent = (exponent << 8) | int(b)
		}
		n := new(big.Int).SetBytes(modulus)
		if n.BitLen() < 2048 || exponent < 3 || exponent%2 == 0 {
			continue
		}
		keys[rawKey.KeyID] = &rsa.PublicKey{N: n, E: exponent}
	}
	if len(keys) == 0 {
		return nil, ErrGoogleResponse
	}
	maxAge := time.Hour
	for _, directive := range strings.Split(resp.Header.Get("Cache-Control"), ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(directive), "=")
		if !ok || !strings.EqualFold(name, "max-age") {
			continue
		}
		seconds, parseErr := strconv.Atoi(strings.Trim(value, `"`))
		if parseErr == nil && seconds > 0 && seconds < int((24*time.Hour)/time.Second) {
			maxAge = time.Duration(seconds) * time.Second
		}
	}
	v.mu.Lock()
	v.keys, v.keyExp = keys, time.Now().Add(maxAge)
	key := v.keys[kid]
	v.mu.Unlock()
	if key == nil {
		return nil, fmt.Errorf("%w: signing key not found", ErrGoogleToken)
	}
	return key, nil
}

// NewPKCEVerifier returns a high-entropy verifier suitable for S256 PKCE.
func NewPKCEVerifier() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}
