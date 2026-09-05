package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

const (
	ClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	AuthorizeURL   = "https://auth.openai.com/oauth/authorize"
	TokenURL       = "https://auth.openai.com/oauth/token"
	Scope          = "openid profile email offline_access"
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	Originator     = "codex_cli_rs"
	UserAgent      = "codex_cli_rs/0.136.0"
)

func AuthorizationURL(redirectURI, state, challenge string) (string, error) {
	if redirectURI == "" || state == "" || challenge == "" {
		return "", errors.New("codex OAuth parameters are required")
	}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", Scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", Originator)
	q.Set("state", state)
	// The reference client uses %20 rather than form-style '+' for spaces.
	return AuthorizeURL + "?" + strings.ReplaceAll(q.Encode(), "+", "%20"), nil
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
