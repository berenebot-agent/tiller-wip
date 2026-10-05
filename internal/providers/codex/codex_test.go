package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// routingTransport redirects every request to a single mock server, so the
// package-level issuer URLs (including the const TokenURL) can be exercised
// without network access.
type routingTransport struct {
	server *httptest.Server
}

func (t *routingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.server.URL, "http://")
	return http.DefaultTransport.RoundTrip(req)
}

func TestRequestDeviceCodeSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/deviceauth/usercode" {
			http.Error(w, "unexpected path", http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("originator"); got != Originator {
			t.Errorf("originator header = %q, want %q", got, Originator)
		}
		if got := r.Header.Get("User-Agent"); got != UserAgent {
			t.Errorf("User-Agent header = %q, want %q", got, UserAgent)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_auth_id":"deviceauth_abc","user_code":"48HT-E45RX","interval":"5","expires_at":"` + time.Now().Add(15*time.Minute).UTC().Format(time.RFC3339) + `"}`))
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	device, err := RequestDeviceCode(context.Background(), client)
	if err != nil {
		t.Fatalf("RequestDeviceCode error = %v", err)
	}
	if device.DeviceAuthID != "deviceauth_abc" {
		t.Errorf("DeviceAuthID = %q, want deviceauth_abc", device.DeviceAuthID)
	}
	if device.UserCode != "48HT-E45RX" {
		t.Errorf("UserCode = %q, want 48HT-E45RX", device.UserCode)
	}
	if device.VerificationURI != DeviceVerificationURL {
		t.Errorf("VerificationURI = %q, want %q", device.VerificationURI, DeviceVerificationURL)
	}
	if device.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want 5s", device.Interval)
	}
	if device.ExpiresIn <= 0 || device.ExpiresIn > int64((15*time.Minute)/time.Second) {
		t.Errorf("ExpiresIn = %d, want within (0, 900]", device.ExpiresIn)
	}
}

func TestRequestDeviceCodeAcceptedAliasAndDefaultInterval(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_auth_id":"deviceauth_abc","usercode":"ABCD-EFGHI"}`))
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	device, err := RequestDeviceCode(context.Background(), client)
	if err != nil {
		t.Fatalf("RequestDeviceCode error = %v", err)
	}
	if device.UserCode != "ABCD-EFGHI" {
		t.Errorf("UserCode = %q, want ABCD-EFGHI", device.UserCode)
	}
	if device.Interval != 5*time.Second {
		t.Errorf("Interval = %v, want default 5s", device.Interval)
	}
}

func TestRequestDeviceCodeUnsupported(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	_, err := RequestDeviceCode(context.Background(), client)
	if !errors.Is(err, ErrDeviceCodeUnsupported) {
		t.Fatalf("err = %v, want ErrDeviceCodeUnsupported", err)
	}
}

func TestPollDeviceTokenPendingThenExchanges(t *testing.T) {
	polls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"authorization_code":"auth-code-123","code_challenge":"chal","code_verifier":"verifier-456"}`))
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			if got := r.PostForm.Get("redirect_uri"); got != DeviceRedirectURI {
				t.Errorf("redirect_uri = %q, want %q", got, DeviceRedirectURI)
			}
			if got := r.PostForm.Get("code_verifier"); got != "verifier-456" {
				t.Errorf("code_verifier = %q, want verifier-456", got)
			}
			if got := r.PostForm.Get("code"); got != "auth-code-123" {
				t.Errorf("code = %q, want auth-code-123", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.Error(w, "unexpected path", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	device := DeviceCode{DeviceAuthID: "deviceauth_abc", UserCode: "48HT-E45RX", ExpiresIn: 30, Interval: 10 * time.Millisecond}
	tokens, err := PollDeviceToken(context.Background(), client, device)
	if err != nil {
		t.Fatalf("PollDeviceToken error = %v", err)
	}
	if tokens.AccessToken != "access-1" || tokens.RefreshToken != "refresh-1" {
		t.Errorf("tokens = %+v, want access-1/refresh-1", tokens)
	}
	if polls != 2 {
		t.Errorf("polls = %d, want 2 (pending then approved)", polls)
	}
}

func TestPollDeviceTokenExchangeFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"authorization_code":"auth-code-123","code_verifier":"verifier-456"}`))
		case "/oauth/token":
			http.Error(w, "bad code", http.StatusBadRequest)
		default:
			http.Error(w, "unexpected path", http.StatusBadRequest)
		}
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	device := DeviceCode{DeviceAuthID: "deviceauth_abc", UserCode: "48HT-E45RX", ExpiresIn: 30, Interval: 10 * time.Millisecond}
	if _, err := PollDeviceToken(context.Background(), client, device); err == nil {
		t.Fatal("expected exchange failure, got nil")
	}
}
