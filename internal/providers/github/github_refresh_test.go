package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

// routingTransport redirects all HTTP requests to a single mock server.
type routingTransport struct {
	server *httptest.Server
}

func (t *routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

// TestRefreshCopilot500WithRefreshTokenStaysTransient verifies that a transient
// Copilot-token failure (500) with a valid refresh token returns a transient
// error and does NOT call the GitHub refresh endpoint.
func TestRefreshCopilot500WithRefreshTokenStaysTransient(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		// Copilot token endpoint returns 500 (transient).
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	current := oauth.TokenRecord{
		AccessToken:  "dead-access",
		RefreshToken: "valid-refresh-token",
		TokenType:    "Bearer",
		AuthState:    oauth.AuthConnected,
	}

	_, err := Refresh(context.Background(), client, current)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err == oauth.ErrReconnectRequired {
		t.Fatal("500 should not return ErrReconnectRequired")
	}
	// Only the Copilot endpoint should have been called — not the GitHub
	// refresh endpoint.
	if requests != 1 {
		t.Fatalf("expected 1 request to Copilot endpoint, got %d", requests)
	}
}

// TestRefreshCopilot401WithNoRefreshTokenReturnsReconnectRequired verifies that
// a 401 from the Copilot token endpoint with no GitHub refresh token is
// classified as reconnect_required.
func TestRefreshCopilot401WithNoRefreshTokenReturnsReconnectRequired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	current := oauth.TokenRecord{
		AccessToken:  "dead-access",
		RefreshToken: "", // no GitHub refresh token
		TokenType:    "Bearer",
		AuthState:    oauth.AuthConnected,
	}

	_, err := Refresh(context.Background(), client, current)
	if err != oauth.ErrReconnectRequired {
		t.Fatalf("Copilot 401 without refresh token: err = %v, want ErrReconnectRequired", err)
	}
}

// TestRefreshCopilot401WithValidRefreshTokenRefreshesAndSucceeds verifies that
// a 401 from the Copilot token endpoint with a valid GitHub refresh token
// triggers a durable refresh and returns a fresh Copilot token.
func TestRefreshCopilot401WithValidRefreshTokenRefreshesAndSucceeds(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/copilot_internal/v2/token" && requests == 1 {
			// First Copilot call: 401 (dead access token).
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/copilot_internal/v2/token" && requests == 3 {
			// Second Copilot call (after refresh): success.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "fresh-copilot-token", "expires_at": time.Now().Add(time.Hour).Unix()})
			return
		}
		if r.URL.Path == "/login/oauth/access_token" && requests == 2 {
			// GitHub refresh: success.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "fresh-github-access",
				"refresh_token": "rotated-refresh-token",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
			return
		}
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	current := oauth.TokenRecord{
		AccessToken:  "dead-access",
		RefreshToken: "valid-refresh-token",
		TokenType:    "Bearer",
		AuthState:    oauth.AuthConnected,
	}

	result, err := Refresh(context.Background(), client, current)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if result.AccessToken != "fresh-github-access" {
		t.Fatalf("access_token = %q, want fresh-github-access", result.AccessToken)
	}
	if result.RefreshToken != "rotated-refresh-token" {
		t.Fatalf("refresh_token = %q, want rotated-refresh-token", result.RefreshToken)
	}
	if requests != 3 {
		t.Fatalf("expected 3 requests (copilot 401, github refresh, copilot 200), got %d", requests)
	}
}

// TestRefreshGitHubInvalidGrantReturnsReconnectRequired verifies that an
// invalid_grant from the GitHub refresh-token endpoint is classified as
// reconnect_required.
func TestRefreshGitHubInvalidGrantReturnsReconnectRequired(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/copilot_internal/v2/token" && requests == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/login/oauth/access_token" && requests == 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	current := oauth.TokenRecord{
		AccessToken:  "dead-access",
		RefreshToken: "dead-refresh-token",
		TokenType:    "Bearer",
		AuthState:    oauth.AuthConnected,
	}

	_, err := Refresh(context.Background(), client, current)
	if err != oauth.ErrReconnectRequired {
		t.Fatalf("GitHub invalid_grant: err = %v, want ErrReconnectRequired", err)
	}
	if requests != 2 {
		t.Fatalf("expected 2 requests (copilot 401, github invalid_grant), got %d", requests)
	}
}
