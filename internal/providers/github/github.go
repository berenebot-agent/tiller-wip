package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

const (
	ClientID        = "Iv1.b507a08c87ecfe98"
	DeviceCodeURL   = "https://github.com/login/device/code"
	TokenURL        = "https://github.com/login/oauth/access_token"
	UserInfoURL     = "https://api.github.com/user"
	CopilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	DefaultBaseURL  = "https://api.githubcopilot.com"
	UserAgent       = "GitHubCopilotChat/0.26.7"
)

type DeviceCode struct {
	DeviceCode, UserCode, VerificationURI, VerificationURIComplete string
	ExpiresIn                                                      int64
	Interval                                                       time.Duration
}
type User struct {
	ID                 int64 `json:"id"`
	Login, Name, Email string
}

func RequestDeviceCode(ctx context.Context, client *http.Client) (DeviceCode, error) {
	form := url.Values{"client_id": {ClientID}, "scope": {"read:user"}}
	var response struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	if err := requestJSON(ctx, client, http.MethodPost, DeviceCodeURL, form, nil, &response); err != nil {
		return DeviceCode{}, err
	}
	if response.DeviceCode == "" || response.UserCode == "" {
		return DeviceCode{}, errors.New("invalid GitHub device authorization response")
	}
	interval := time.Duration(response.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return DeviceCode{response.DeviceCode, response.UserCode, response.VerificationURI, response.VerificationURIComplete, response.ExpiresIn, interval}, nil
}

func PollToken(ctx context.Context, client *http.Client, device DeviceCode) (oauth.TokenResponse, error) {
	expires := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	result, err := oauth.PollDeviceCode(ctx, expires, device.Interval, func(pollCtx context.Context) oauth.DevicePollResult {
		form := url.Values{"client_id": {ClientID}, "device_code": {device.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
		var response struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			TokenType    string `json:"token_type"`
			Scope        string `json:"scope"`
			Error        string `json:"error"`
		}
		_, requestErr := requestJSONStatus(pollCtx, client, http.MethodPost, TokenURL, form, &response, nil)
		if requestErr != nil {
			return oauth.DevicePollResult{Err: requestErr}
		}
		if response.AccessToken != "" {
			return oauth.DevicePollResult{Status: oauth.DeviceSuccess, Token: oauth.TokenResponse{AccessToken: response.AccessToken, RefreshToken: response.RefreshToken, TokenType: response.TokenType, Scope: response.Scope}}
		}
		switch response.Error {
		case "authorization_pending":
			return oauth.DevicePollResult{Status: oauth.DevicePending}
		case "slow_down":
			return oauth.DevicePollResult{Status: oauth.DeviceSlowDown}
		case "expired_token":
			return oauth.DevicePollResult{Status: oauth.DeviceExpired}
		case "access_denied":
			return oauth.DevicePollResult{Status: oauth.DeviceDenied}
		default:
			return oauth.DevicePollResult{Err: errors.New("GitHub device authorization failed")}
		}
	})
	return result, err
}

func FetchUser(ctx context.Context, client *http.Client, accessToken string) (User, error) {
	var user User
	if err := requestJSON(ctx, client, http.MethodGet, UserInfoURL, nil, map[string]string{"Authorization": "Bearer " + accessToken, "X-GitHub-Api-Version": "2022-11-28", "User-Agent": UserAgent}, &user); err != nil {
		return User{}, err
	}
	return user, nil
}

func FetchCopilotToken(ctx context.Context, client *http.Client, accessToken string) (oauth.TokenResponse, int, error) {
	var response struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	status, err := requestJSONStatus(ctx, client, http.MethodGet, CopilotTokenURL, nil, &response, map[string]string{"Authorization": "Bearer " + accessToken, "Accept": "application/json", "X-GitHub-Api-Version": "2025-04-01", "User-Agent": UserAgent, "Editor-Version": "vscode/1.85.0", "Editor-Plugin-Version": "copilot-chat/0.26.7"})
	if err != nil {
		return oauth.TokenResponse{}, status, err
	}
	if response.Token == "" {
		return oauth.TokenResponse{}, status, errors.New("GitHub Copilot token was empty")
	}
	result := oauth.TokenResponse{ProviderData: map[string]any{"copilot_token": response.Token, "copilot_token_expires_at": response.ExpiresAt}}
	if response.ExpiresAt > 0 {
		result.ExpiresIn = response.ExpiresAt - time.Now().Unix()
		if result.ExpiresIn <= 0 {
			result.ExpiresIn = -1
		}
	}
	return result, status, nil
}

func Refresh(ctx context.Context, client *http.Client, current oauth.TokenRecord) (oauth.TokenResponse, error) {
	token, status, err := FetchCopilotToken(ctx, client, current.AccessToken)
	if err == nil {
		token.AccessToken = current.AccessToken
		token.RefreshToken = current.RefreshToken
		token.TokenType = current.TokenType
		token.Scope = current.Scope
		token.IDToken = current.IDToken
		token.AccountEmail = current.AccountEmail
		token.AccountPlan = current.AccountPlan
		return token, nil
	}
	// A 401/403 from the Copilot token endpoint means the access token is
	// dead. If a GitHub refresh token exists, try to rotate the durable
	// credential and fetch a fresh Copilot token. Only if no refresh token
	// is available do we ask the user to reconnect.
	if status == 401 || status == 403 {
		if current.RefreshToken == "" {
			return oauth.TokenResponse{}, oauth.ErrReconnectRequired
		}
		return refreshGitHubToken(ctx, client, current)
	}
	// Any other failure (network blip, 429, 5xx, timeout) is transient.
	// Do not rotate a perfectly good refresh token for a recoverable error.
	return oauth.TokenResponse{}, err
}

// refreshGitHubToken rotates the durable GitHub access token using the stored
// refresh token, then fetches a fresh Copilot token. It classifies the GitHub
// refresh-token response: invalid_grant / 401 / 403 → reconnect_required;
// anything else → transient.
func refreshGitHubToken(ctx context.Context, client *http.Client, current oauth.TokenRecord) (oauth.TokenResponse, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {current.RefreshToken}, "client_id": {ClientID}}
	var githubToken struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	status, body, err := requestJSONStatusWithBody(ctx, client, http.MethodPost, TokenURL, form, nil)
	if err != nil {
		// Parse the OAuth error body to detect a dead refresh token.
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errResp)
		if errResp.Error == "invalid_grant" || status == 401 || status == 403 {
			return oauth.TokenResponse{}, oauth.ErrReconnectRequired
		}
		return oauth.TokenResponse{}, err
	}
	if err := json.Unmarshal(body, &githubToken); err != nil {
		return oauth.TokenResponse{}, errors.New("invalid GitHub refresh response")
	}
	copilot, _, err := FetchCopilotToken(ctx, client, githubToken.AccessToken)
	if err != nil {
		return oauth.TokenResponse{}, err
	}
	copilot.AccessToken, copilot.RefreshToken, copilot.TokenType, copilot.ExpiresIn, copilot.Scope = githubToken.AccessToken, githubToken.RefreshToken, githubToken.TokenType, githubToken.ExpiresIn, githubToken.Scope
	return copilot, nil
}

// requestJSONStatusWithBody is like requestJSONStatus but also returns the
// raw response body so callers can parse OAuth error details (e.g.
// invalid_grant) from non-2xx responses.
func requestJSONStatusWithBody(ctx context.Context, client *http.Client, method, endpoint string, form url.Values, headers map[string]string) (int, []byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	var body strings.Reader
	if form != nil {
		body = *strings.NewReader(form.Encode())
	} else {
		body = *strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, &body)
	if err != nil {
		return 0, nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	if headers != nil {
		for key, value := range headers {
			req.Header.Set(key, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, raw, errors.New("GitHub API request failed")
	}
	return resp.StatusCode, raw, nil
}

func requestJSON(ctx context.Context, client *http.Client, method, endpoint string, form url.Values, headers map[string]string, target any) error {
	_, err := requestJSONStatus(ctx, client, method, endpoint, form, target, headers)
	return err
}
func requestJSONStatus(ctx context.Context, client *http.Client, method, endpoint string, form url.Values, target any, optional ...map[string]string) (int, error) {
	if client == nil {
		client = http.DefaultClient
	}
	var body strings.Reader
	if form != nil {
		body = *strings.NewReader(form.Encode())
	} else {
		body = *strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, &body)
	if err != nil {
		return 0, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	if len(optional) > 0 {
		for key, value := range optional[0] {
			req.Header.Set(key, value)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, errors.New("GitHub API request failed")
	}
	if target != nil && json.NewDecoder(resp.Body).Decode(target) != nil {
		return resp.StatusCode, errors.New("invalid GitHub API response")
	}
	return resp.StatusCode, nil
}
