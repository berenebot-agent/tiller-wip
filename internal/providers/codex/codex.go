package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

const (
	ClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	TokenURL       = "https://auth.openai.com/oauth/token"
	Scope          = "openid profile email offline_access"
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	Originator     = "codex_cli_rs"
	// ClientVersion is the floor client version Tiller advertises when the live
	// release channel cannot be reached. Codex model discovery gates each model
	// on its minimal_client_version, so a stale floor silently hides newer
	// models; discovery resolves the current version at runtime and only falls
	// back to this value.
	ClientVersion = "0.156.1"
	UserAgent     = "codex_cli_rs/" + ClientVersion

	// Device authorization is the sign-in flow OpenAI provides for remote or
	// headless clients (see `codex login --device-auth`). It requires no
	// client-supplied redirect URI: the token exchange uses OpenAI's own
	// registered DeviceRedirectURI, so it is the only Codex sign-in path that
	// works from a hosted server. The PKCE browser flow's redirect URI must be
	// the registered loopback callback, which a hosted router cannot observe.
	Issuer                = "https://auth.openai.com"
	DeviceVerificationURL = Issuer + "/codex/device"
	DeviceRedirectURI     = Issuer + "/deviceauth/callback"
	devicePollTimeout     = 15 * time.Minute
)

// ErrDeviceCodeUnsupported reports that the issuer has device authorization
// disabled. The reference CLI treats this as a signal to fall back to browser
// sign-in.
var ErrDeviceCodeUnsupported = errors.New("codex device authorization is not enabled")

// ReleaseChannelURL is the OpenAI-owned release channel the Codex installer
// uses to resolve "latest". It reports {"tag_name":"rust-v0.156.1",...}. It is
// a var so tests can point it at a local server.
var ReleaseChannelURL = "https://releases.openai.com/codex/channels/latest"

// DeviceUsercodeURL and DeviceTokenURL are vars so tests can point them at a
// local server.
var (
	DeviceUsercodeURL = Issuer + "/api/accounts/deviceauth/usercode"
	DeviceTokenURL    = Issuer + "/api/accounts/deviceauth/token"
)

// ResolveLatestVersion fetches the latest Codex CLI release version from the
// OpenAI release channel and returns it in bare x.y.z form (the channel reports
// tag_name as "rust-v0.156.1"). Callers treat any error as non-fatal and fall
// back to ClientVersion, so a release-channel outage never fails a refresh.
func ResolveLatestVersion(ctx context.Context, client *http.Client) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleaseChannelURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("codex release channel returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode codex release channel: %w", err)
	}
	version := strings.TrimPrefix(payload.TagName, "rust-v")
	if !validCodexVersion(version) {
		return "", fmt.Errorf("codex release channel returned invalid tag %q", payload.TagName)
	}
	return version, nil
}

// validCodexVersion accepts the x.y.z core the Codex models endpoint requires as
// client_version, tolerating a pre-release suffix (e.g. 0.157.0-alpha.11).
func validCodexVersion(version string) bool {
	core, _, _ := strings.Cut(version, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// DeviceCode is the in-progress device authorization returned by
// RequestDeviceCode. VerificationURI is where the user approves the sign-in.
type DeviceCode struct {
	DeviceAuthID    string
	UserCode        string
	VerificationURI string
	ExpiresIn       int64
	Interval        time.Duration
}

// RequestDeviceCode starts the device authorization flow. A 404 from the issuer
// means device authorization is disabled and the caller should surface that
// rather than retrying.
func RequestDeviceCode(ctx context.Context, client *http.Client) (DeviceCode, error) {
	if client == nil {
		client = http.DefaultClient
	}
	payload, err := json.Marshal(map[string]string{"client_id": ClientID})
	if err != nil {
		return DeviceCode{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DeviceUsercodeURL, strings.NewReader(string(payload)))
	if err != nil {
		return DeviceCode{}, err
	}
	setDeviceHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return DeviceCode{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return DeviceCode{}, ErrDeviceCodeUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return DeviceCode{}, fmt.Errorf("codex device authorization request failed with HTTP %d", resp.StatusCode)
	}
	var result struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		UserCodeAlt  string `json:"usercode"`
		Interval     string `json:"interval"`
		ExpiresAt    string `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return DeviceCode{}, errors.New("invalid codex device authorization response")
	}
	userCode := result.UserCode
	if userCode == "" {
		userCode = result.UserCodeAlt
	}
	if result.DeviceAuthID == "" || userCode == "" {
		return DeviceCode{}, errors.New("invalid codex device authorization response")
	}
	interval := 5 * time.Second
	if seconds, err := strconv.Atoi(strings.TrimSpace(result.Interval)); err == nil && seconds > 0 {
		interval = time.Duration(seconds) * time.Second
	}
	expiresIn := int64(devicePollTimeout / time.Second)
	if expiresAt, err := time.Parse(time.RFC3339, result.ExpiresAt); err == nil {
		if remaining := int64(time.Until(expiresAt).Seconds()); remaining > 0 {
			expiresIn = remaining
		}
	}
	return DeviceCode{DeviceAuthID: result.DeviceAuthID, UserCode: userCode, VerificationURI: DeviceVerificationURL, ExpiresIn: expiresIn, Interval: interval}, nil
}

// PollDeviceToken polls until the user approves the device code, then exchanges
// the returned authorization code for tokens using the PKCE verifier the issuer
// supplies. It returns the same token shape as the browser flow, so the caller's
// persistence path is unchanged.
func PollDeviceToken(ctx context.Context, client *http.Client, device DeviceCode) (oauth.TokenResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	expiresIn := device.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = int64(devicePollTimeout / time.Second)
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)
	return oauth.PollDeviceCode(ctx, expiresAt, device.Interval, func(pollCtx context.Context) oauth.DevicePollResult {
		payload, err := json.Marshal(map[string]string{"device_auth_id": device.DeviceAuthID, "user_code": device.UserCode})
		if err != nil {
			return oauth.DevicePollResult{Err: err}
		}
		req, err := http.NewRequestWithContext(pollCtx, http.MethodPost, DeviceTokenURL, strings.NewReader(string(payload)))
		if err != nil {
			return oauth.DevicePollResult{Err: err}
		}
		setDeviceHeaders(req)
		resp, err := client.Do(req)
		if err != nil {
			return oauth.DevicePollResult{Err: err}
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return oauth.DevicePollResult{Status: oauth.DevicePending}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return oauth.DevicePollResult{Err: fmt.Errorf("codex device authorization failed with HTTP %d", resp.StatusCode)}
		}
		var result struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			return oauth.DevicePollResult{Err: errors.New("invalid codex device token response")}
		}
		if result.AuthorizationCode == "" {
			return oauth.DevicePollResult{Err: errors.New("codex device authorization returned no code")}
		}
		tokens, err := Exchange(pollCtx, client, result.AuthorizationCode, DeviceRedirectURI, result.CodeVerifier)
		if err != nil {
			return oauth.DevicePollResult{Err: err}
		}
		return oauth.DevicePollResult{Status: oauth.DeviceSuccess, Token: tokens}
	})
}

func setDeviceHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("originator", Originator)
}

func Exchange(ctx context.Context, client *http.Client, code, redirectURI, verifier string) (oauth.TokenResponse, error) {
	return tokenRequest(ctx, client, url.Values{"grant_type": {"authorization_code"}, "client_id": {ClientID}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier}})
}

func Refresh(ctx context.Context, client *http.Client, refreshToken string) (oauth.TokenResponse, error) {
	return tokenRequest(ctx, client, url.Values{"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {refreshToken}, "scope": {Scope}})
}

func tokenRequest(ctx context.Context, client *http.Client, form url.Values) (oauth.TokenResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauth.TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return oauth.TokenResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Parse the error response to detect a dead refresh token (OAuth 2.0
		// invalid_grant). This lets the routing layer transition the saved
		// token into reconnect_required instead of retrying forever.
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error == "invalid_grant" || resp.StatusCode == 401 || resp.StatusCode == 403 {
			return oauth.TokenResponse{}, oauth.ErrReconnectRequired
		}
		return oauth.TokenResponse{}, errors.New("codex OAuth token request failed")
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		IDToken      string `json:"id_token"`
		Scope        string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return oauth.TokenResponse{}, errors.New("invalid codex OAuth token response")
	}
	account := AccountInfo(result.IDToken)
	return oauth.TokenResponse{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, TokenType: result.TokenType, ExpiresIn: result.ExpiresIn, IDToken: result.IDToken, Scope: result.Scope, AccountEmail: account.Email, AccountPlan: account.Plan}, nil
}

type Account struct{ Email, ID, Plan string }

func AccountInfo(idToken string) Account {
	payload := extractUnverifiedJWTClaims(idToken)
	auth, _ := payload["https://api.openai.com/auth"].(map[string]any)
	return Account{Email: stringValue(payload["email"]), ID: firstString(auth["chatgpt_account_id"], payload["account_id"]), Plan: firstString(auth["chatgpt_plan_type"], payload["plan_type"])}
}

// extractUnverifiedJWTClaims reads display metadata; callers must not use it for authentication.
func extractUnverifiedJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	var payload map[string]any
	if json.Unmarshal(b, &payload) != nil {
		return map[string]any{}
	}
	return payload
}

func stringValue(value any) string { result, _ := value.(string); return result }
func firstString(values ...any) string {
	for _, value := range values {
		if result := stringValue(value); result != "" {
			return result
		}
	}
	return ""
}
