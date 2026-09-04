package claude

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

// TestRefreshInvalidGrantReturnsReconnectRequired verifies that an invalid_grant
// error from the Claude token endpoint is classified as reconnect_required.
func TestRefreshInvalidGrantReturnsReconnectRequired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	_, err := Refresh(context.Background(), client, "dead-refresh-token")
	if err != oauth.ErrReconnectRequired {
		t.Fatalf("invalid_grant: err = %v, want ErrReconnectRequired", err)
	}
}

// TestRefresh401ReturnsReconnectRequired verifies that a 401 from the Claude
// token endpoint is classified as reconnect_required.
func TestRefresh401ReturnsReconnectRequired(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	_, err := Refresh(context.Background(), client, "dead-refresh-token")
	if err != oauth.ErrReconnectRequired {
		t.Fatalf("401: err = %v, want ErrReconnectRequired", err)
	}
}

// TestRefresh500StaysTransient verifies that a 500 from the Claude token endpoint
// stays as a transient error (not reconnect_required).
func TestRefresh500StaysTransient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	_, err := Refresh(context.Background(), client, "good-refresh-token")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err == oauth.ErrReconnectRequired {
		t.Fatal("500 should not return ErrReconnectRequired")
	}
}
