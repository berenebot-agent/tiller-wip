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
	UserAgent      = "claude-cli/2.1.251 (external, sdk-cli)"
	BetaHeader     = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14"
)

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
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !u.IsAbs() || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return oauth.Callback{}, oauth.ErrCallback
	}
	query := u.Query()
	code := strings.TrimSpace(query.Get("code"))
	state := strings.TrimSpace(query.Get("state"))
	if code == "" && u.Fragment != "" {
		code = strings.TrimSpace(query.Get("code"))
	}
	if code == "" || state == "" {
		return oauth.Callback{}, oauth.ErrCallback
	}
	if hash := strings.IndexByte(code, '#'); hash >= 0 {
		if state == "" {
			state = strings.TrimSpace(code[hash+1:])
		}
		code = code[:hash]
	}
	return oauth.Callback{Code: code, State: state}, nil
}

func Exchange(ctx context.Context, client *http.Client, code, redirectURI, verifier, state string) (oauth.TokenResponse, error) {
	if hash := strings.IndexByte(code, '#'); hash >= 0 {
		code, state = code[:hash], code[hash+1:]
	}
	body := map[string]string{"code": code, "grant_type": "authorization_code", "client_id": ClientID, "redirect_uri": redirectURI, "code_verifier": verifier}
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
