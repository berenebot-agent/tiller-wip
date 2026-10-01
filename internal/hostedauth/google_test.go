package hostedauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

const testGoogleClientID = "test-client.apps.googleusercontent.com"

// testVerifier returns a verifier whose signing key is already cached, so
// ValidateIDToken never reaches the network. kid is the key identifier embedded
// in the token header.
func testVerifier(t *testing.T, kid string) (*GoogleVerifier, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	verifier := NewGoogleVerifier(&http.Client{})
	verifier.keys = map[string]*rsa.PublicKey{kid: &key.PublicKey}
	verifier.keyExp = time.Now().Add(time.Hour)
	return verifier, key
}

// signToken builds an RS256 ID token with the given claims signed by key.
func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := b64(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload := b64(claims)
	signed := header + "." + payload
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func baseClaims(nonce string) map[string]any {
	now := time.Now()
	claims := map[string]any{
		"iss":            "https://accounts.google.com",
		"sub":            "109676735841593478562",
		"aud":            testGoogleClientID,
		"azp":            testGoogleClientID,
		"email":          "user@example.com",
		"email_verified": true,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	return claims
}

func TestValidateIDTokenNonce(t *testing.T) {
	const kid = "test-kid"
	cases := []struct {
		name     string
		nonce    string // nonce embedded in the token ("" omits the claim)
		want     string // nonce the caller expects
		wantGood bool
	}{
		{"gsi sentinel accepted when no nonce requested", "not_provided", "", true},
		{"absent nonce accepted when no nonce requested", "", "", true},
		{"unexpected nonce rejected when no nonce requested", "real-nonce", "", false},
		{"redirect flow exact match accepted", "real-nonce", "real-nonce", true},
		{"redirect flow mismatch rejected", "other-nonce", "real-nonce", false},
		{"redirect flow missing nonce rejected", "", "real-nonce", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verifier, key := testVerifier(t, kid)
			token := signToken(t, key, kid, baseClaims(tc.nonce))
			_, err := verifier.ValidateIDToken(context.Background(), token, testGoogleClientID, tc.want)
			if tc.wantGood && err != nil {
				t.Fatalf("ValidateIDToken() error = %v, want nil", err)
			}
			if !tc.wantGood && err == nil {
				t.Fatalf("ValidateIDToken() = nil, want %v", ErrGoogleToken)
			}
		})
	}
}

func TestValidateIDTokenRejectsBadSignature(t *testing.T) {
	const kid = "test-kid"
	verifier, _ := testVerifier(t, kid)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	token := signToken(t, otherKey, kid, baseClaims("not_provided"))
	if _, err := verifier.ValidateIDToken(context.Background(), token, testGoogleClientID, ""); err == nil {
		t.Fatalf("ValidateIDToken() accepted a token signed by another key")
	}
}

func TestValidateIDTokenRejectsAudienceMismatch(t *testing.T) {
	const kid = "test-kid"
	verifier, key := testVerifier(t, kid)
	token := signToken(t, key, kid, baseClaims("not_provided"))
	if _, err := verifier.ValidateIDToken(context.Background(), token, "other-client.apps.googleusercontent.com", ""); err == nil {
		t.Fatalf("ValidateIDToken() accepted a token for a different audience")
	}
}
