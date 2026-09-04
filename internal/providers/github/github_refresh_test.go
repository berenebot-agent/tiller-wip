package github

import (
	"context"
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

// TestRefreshCopilotTransientWithoutRefreshToken verifies that a transient
// Copilot-token failure with no GitHub refresh token returns a transient
// error (not reconnect_required).
func TestRefreshCopilotTransientWithoutRefreshToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Copilot token endpoint returns 500 (transient).
		w.WriteHeader(http.StatusInternalServerError)
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
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Must NOT be reconnect_required — this is a transient failure.
	if err == oauth.ErrReconnectRequired {
		t.Fatalf("got ErrReconnectRequired for transient Copilot failure without refresh token; want transient error")
	}
}

// TestRefreshCopilot401ReturnsReconnectRequired verifies that a 401/403 from the
// Copilot token endpoint is classified as reconnect_required (dead token).
func TestRefreshCopilot401ReturnsReconnectRequired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	current := oauth.TokenRecord{
		AccessToken:  "dead-access",
		RefreshToken: "some-refresh-token",
		TokenType:    "Bearer",
		AuthState:    oauth.AuthConnected,
	}

	_, err := Refresh(context.Background(), client, current)
	if err != oauth.ErrReconnectRequired {
		t.Fatalf("Copilot 401: err = %v, want ErrReconnectRequired", err)
	}
}
