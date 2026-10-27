package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

const (
	ClientID       = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	AuthorizeURL   = "https://claude.ai/oauth/authorize"
	TokenURL       = "https://api.anthropic.com/v1/oauth/token"
	DefaultBaseURL = "https://api.anthropic.com/v1"
	CLIVersion     = "2.1.311"
	UserAgent      = "claude-cli/" + CLIVersion + " (external, sdk-cli)"
	BetaHeader     = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"
)

// RedirectCallbackSupported reports whether the provider returns the
// authorization code as a browser redirect the server can observe. Claude's
// flow is designed for manual copy (the code is shown to the user, possibly as
// a code#state fragment), and a URL fragment is never sent to the server, so
// hosted deployments keep paste-back for claude-subscription. This is a
// provider-scoped compatibility fact, not a model-ID guess.
const RedirectCallbackSupported = false

// ManualRedirectURI is the redirect URI registered for the Claude Code OAuth
// client that displays the authorization code (as code#state) for the user to
// paste back. The client rejects any other host, including the router's own
// /auth/callback, with "Redirect URI ... is not supported by client".
const ManualRedirectURI = "https://platform.claude.com/oauth/code/callback"

var Scopes = []string{"org:create_api_key", "user:profile", "user:inference"}

func AuthorizationURL(redirectURI, state, challenge string) (string, error) {
	if redirectURI == "" || state == "" || challenge == "" {
		return "", errors.New("claude OAuth parameters are required")
	}
	q := url.Values{}
	q.Set("code", "true")
	q.Set("client_id", ClientID)
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(Scopes, " "))
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	return AuthorizeURL + "?" + strings.ReplaceAll(q.Encode(), "+", "%20"), nil
}

func ParseCallback(raw string) (oauth.Callback, error) {
	raw = strings.TrimSpace(raw)
	// The manual redirect page shows a bare "<code>#<state>" string, not a URL.
	if !strings.Contains(raw, "://") {
		code, state, ok := strings.Cut(raw, "#")
		code, state = strings.TrimSpace(code), strings.TrimSpace(state)
		if !ok || code == "" || state == "" || strings.ContainsAny(raw, " \t\r\n?&") {
			return oauth.Callback{}, oauth.ErrCallback
		}
		return oauth.Callback{Code: code, State: state}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return oauth.Callback{}, oauth.ErrCallback
	}
	query := u.Query()
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	// The code itself may carry the state as a fragment-style suffix when the
	// paste kept the two glued together.
	if hash := strings.IndexByte(code, '#'); hash >= 0 {
		if state == "" {
			state = strings.TrimSpace(code[hash+1:])
		}
		code = code[:hash]
	}
	// Claude's code=true flow returns <redirect>?code=<code>#<state>, where
	// url.Parse puts the state in the fragment rather than the query.
	if state == "" {
		state = strings.TrimSpace(u.Fragment)
	}
	if code == "" || state == "" {
		return oauth.Callback{}, oauth.ErrCallback
	}
	return oauth.Callback{Code: code, State: state}, nil
}

func Exchange(ctx context.Context, client *http.Client, code, redirectURI, verifier, state string) (oauth.TokenResponse, error) {
	if hash := strings.IndexByte(code, '#'); hash >= 0 {
		code, state = code[:hash], code[hash+1:]
	}
	body := map[string]string{"code": code, "grant_type": "authorization_code", "client_id": ClientID, "redirect_uri": redirectURI, "code_verifier": verifier, "state": state}
	return tokenRequest(ctx, client, http.MethodPost, TokenURL, "application/json", body)
}

func Refresh(ctx context.Context, client *http.Client, refreshToken string) (oauth.TokenResponse, error) {
	return tokenRequest(ctx, client, http.MethodPost, TokenURL, "application/json", map[string]string{"grant_type": "refresh_token", "client_id": ClientID, "refresh_token": refreshToken})
}

func tokenRequest(ctx context.Context, client *http.Client, method, endpoint, contentType string, payload map[string]string) (oauth.TokenResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return oauth.TokenResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(b)))
	if err != nil {
		return oauth.TokenResponse{}, err
	}
	req.Header.Set("Content-Type", contentType)
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
		return oauth.TokenResponse{}, errors.New("claude OAuth token request failed")
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if json.NewDecoder(resp.Body).Decode(&token) != nil {
		return oauth.TokenResponse{}, errors.New("invalid claude OAuth token response")
	}
	return oauth.TokenResponse{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, TokenType: token.TokenType, ExpiresIn: token.ExpiresIn, Scope: token.Scope}, nil
}
