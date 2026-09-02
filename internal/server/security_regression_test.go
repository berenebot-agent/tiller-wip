package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

func TestClientIPTrustBoundary(t *testing.T) {
	trusted := netip.MustParsePrefix("172.18.0.0/16")
	tests := []struct {
		name, remote, forwarded string
		trusted                 netip.Prefix
		want                    string
	}{
		{name: "trust disabled ignores header", remote: "172.18.0.4:8080", forwarded: "198.51.100.7", trusted: netip.Prefix{}, want: "172.18.0.4"},
		{name: "untrusted peer ignores header", remote: "192.0.2.8:8080", forwarded: "198.51.100.7", trusted: trusted, want: "192.0.2.8"},
		{name: "trusted proxy uses rightmost untrusted hop", remote: "172.18.0.4:8080", forwarded: "198.51.100.7, 172.18.0.9", trusted: trusted, want: "198.51.100.7"},
		{name: "trusted chain walks right to left", remote: "172.18.0.4:8080", forwarded: "198.51.100.7, 172.18.0.9, 172.18.0.10", trusted: trusted, want: "198.51.100.7"},
		{name: "malformed header falls back direct", remote: "172.18.0.4:8080", forwarded: "198.51.100.7, not-an-ip", trusted: trusted, want: "172.18.0.4"},
		{name: "empty header falls back direct", remote: "172.18.0.4:8080", forwarded: "", trusted: trusted, want: "172.18.0.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://router.test/api/admin/session", nil)
			r.RemoteAddr = tt.remote
			r.Header.Set("X-Forwarded-For", tt.forwarded)
			if got := clientIP(r, tt.trusted); got != tt.want {
				t.Fatalf("clientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestClientIPTrustBoundary(t *testing.T) {
	trusted := netip.MustParsePrefix("172.18.0.0/16")
	trustedV6 := netip.MustParsePrefix("2001:db8::/32")
	tests := []struct {
		name, remote, realIP, forwarded string
		trusted                         netip.Prefix
		want                            string
	}{
		{name: "trust disabled ignores headers", remote: "172.18.0.4:8080", forwarded: "198.51.100.7", realIP: "198.51.100.7", trusted: netip.Prefix{}, want: "172.18.0.4"},
		{name: "untrusted peer ignores fake XFF", remote: "192.0.2.8:8080", forwarded: "198.51.100.7", trusted: trusted, want: "192.0.2.8"},
		{name: "untrusted peer ignores fake X-Real-IP", remote: "192.0.2.8:8080", realIP: "198.51.100.7", trusted: trusted, want: "192.0.2.8"},
		{name: "trusted proxy prefers X-Real-IP", remote: "172.18.0.4:8080", realIP: "198.51.100.7", forwarded: "203.0.113.55", trusted: trusted, want: "198.51.100.7"},
		{name: "trusted proxy ignores malicious leftmost XFF", remote: "172.18.0.4:8080", forwarded: "1.2.3.4, 203.0.113.55", trusted: trusted, want: "203.0.113.55"},
		{name: "trusted chain walks right to left", remote: "172.18.0.4:8080", forwarded: "198.51.100.7, 172.18.0.9, 172.18.0.10", trusted: trusted, want: "198.51.100.7"},
		{name: "malformed XFF falls back direct", remote: "172.18.0.4:8080", forwarded: "198.51.100.7, not-an-ip", trusted: trusted, want: "172.18.0.4"},
		{name: "empty XFF falls back direct", remote: "172.18.0.4:8080", forwarded: "", trusted: trusted, want: "172.18.0.4"},
		{name: "IPv6 trusted proxy with IPv6 client", remote: "[2001:db8::1]:8080", forwarded: "2001:db8::1234", trusted: trustedV6, want: "2001:db8::1234"},
		{name: "IPv6 trusted proxy returns rightmost untrusted hop", remote: "[2001:db8::1]:8080", forwarded: "203.0.113.9, 2001:db8::1234", trusted: trustedV6, want: "203.0.113.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := newSecurityTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080", TrustedProxy: tt.trusted})
			r := httptest.NewRequest(http.MethodPost, "http://router.test/v1/chat/completions", nil)
			r.RemoteAddr = tt.remote
			if tt.realIP != "" {
				r.Header.Set("X-Real-IP", tt.realIP)
			}
			if tt.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			if got := app.requestClientIP(r); got != tt.want {
				t.Fatalf("requestClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEveryAdministrativeRouteRequiresAuthentication(t *testing.T) {
	app, _ := newSecurityTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"})
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/admin/session"}, {http.MethodDelete, "/api/admin/session"},
		{http.MethodGet, "/api/admin/provider-types"}, {http.MethodGet, "/api/admin/providers"}, {http.MethodPost, "/api/admin/providers"},
		{http.MethodPatch, "/api/admin/providers/id"}, {http.MethodDelete, "/api/admin/providers/id"}, {http.MethodPut, "/api/admin/providers/id/credential"},
		{http.MethodPost, "/api/admin/providers/id/refresh"}, {http.MethodGet, "/api/admin/providers/id/models"}, {http.MethodGet, "/api/admin/models"},
		{http.MethodGet, "/api/admin/virtual-groups"}, {http.MethodPost, "/api/admin/virtual-groups"}, {http.MethodPatch, "/api/admin/virtual-groups/id"},
		{http.MethodDelete, "/api/admin/virtual-groups/id"}, {http.MethodGet, "/api/admin/virtual-models"}, {http.MethodPost, "/api/admin/virtual-models"},
		{http.MethodPatch, "/api/admin/virtual-models/id"}, {http.MethodDelete, "/api/admin/virtual-models/id"}, {http.MethodGet, "/api/admin/client-keys"},
		{http.MethodPost, "/api/admin/client-keys"}, {http.MethodPatch, "/api/admin/client-keys/id"}, {http.MethodDelete, "/api/admin/client-keys/id"},
		{http.MethodPost, "/api/admin/client-keys/id/rotate"}, {http.MethodGet, "/api/admin/client-keys/id/permissions"}, {http.MethodPut, "/api/admin/client-keys/id/permissions"},
		{http.MethodGet, "/api/admin/client-keys/id/activity"}, {http.MethodGet, "/api/admin/client-keys/id/activity/export"}, {http.MethodDelete, "/api/admin/client-keys/id/activity"},
		{http.MethodGet, "/api/admin/virtual-models/id/activity"}, {http.MethodGet, "/api/admin/virtual-models/id/activity/export"},
		{http.MethodGet, "/api/admin/models/id/activity"}, {http.MethodGet, "/api/admin/models/id/activity/export"},
		{http.MethodGet, "/api/admin/settings"}, {http.MethodPut, "/api/admin/settings"}, {http.MethodPost, "/api/admin/notifications/test"},
		{http.MethodGet, "/api/admin/usage"}, {http.MethodGet, "/api/admin/activity"}, {http.MethodGet, "/api/admin/activity/id/attempts"},
		{http.MethodGet, "/api/admin/health"}, {http.MethodGet, "/api/admin/backup/export"},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			response := httptest.NewRecorder()
			app.Handler().ServeHTTP(response, req)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.Code)
			}
		})
	}
}

func newSecurityTestServer(t *testing.T, cfg config.Config) (*Server, *database.DB) {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return app, db
}

func TestAdminSessionCookieFlagsAndCSRF(t *testing.T) {
	app, _ := newSecurityTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"})
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	request := func(method, path string, body io.Reader, cookie, csrf string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, router.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != "" {
			req.Header.Set("Cookie", sessionCookie+"="+cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	login := request(http.MethodPost, "/api/admin/session", strings.NewReader(`{"username":"admin","password":"correct horse"}`), "", "")
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", login.StatusCode)
	}
	setCookie := login.Header.Get("Set-Cookie")
	if !strings.Contains(setCookie, "HttpOnly") || !strings.Contains(setCookie, "SameSite=Strict") || !strings.Contains(setCookie, "Path=/") {
		t.Fatalf("session cookie missing required flags: %q", setCookie)
	}
	if strings.Contains(setCookie, "; Secure") {
		t.Fatalf("plain HTTP session unexpectedly marked Secure: %q", setCookie)
	}
	var loginPayload struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(login.Body).Decode(&loginPayload); err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(strings.SplitN(setCookie, ";", 2)[0], "=", 2)
	if len(parts) != 2 || parts[1] == "" {
		t.Fatalf("session cookie missing value: %q", setCookie)
	}
	token := parts[1]

	csrfFail := request(http.MethodPut, "/api/admin/settings", strings.NewReader(`{}`), token, "")
	csrfFail.Body.Close()
	if csrfFail.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF token status = %d, want 403", csrfFail.StatusCode)
	}
	logout := request(http.MethodDelete, "/api/admin/session", nil, token, loginPayload.CSRF)
	logout.Body.Close()
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", logout.StatusCode)
	}
	var revoked bool
	for _, cookie := range logout.Header.Values("Set-Cookie") {
		if strings.Contains(cookie, "Max-Age=0") {
			revoked = true
		}
	}
	if !revoked {
		t.Fatalf("logout cookie did not revoke browser cookie: %q", logout.Header.Values("Set-Cookie"))
	}
}

func TestSecureCookieTrustedProxyAndTLS(t *testing.T) {
	trusted := netip.MustParsePrefix("127.0.0.0/8")
	app, _ := newSecurityTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080", TrustedProxy: trusted})
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	req, _ := http.NewRequest(http.MethodPost, router.URL+"/api/admin/session", strings.NewReader(`{"username":"admin","password":"correct horse"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Set-Cookie"), "; Secure") {
		t.Fatalf("trusted HTTPS proxy cookie missing Secure: %q", resp.Header.Get("Set-Cookie"))
	}

	tlsApp, _ := newSecurityTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"})
	tlsRouter := httptest.NewTLSServer(tlsApp.Handler())
	t.Cleanup(tlsRouter.Close)
	tlsClient := tlsRouter.Client()
	tlsClient.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test server certificate
	tlsReq, _ := http.NewRequest(http.MethodPost, tlsRouter.URL+"/api/admin/session", strings.NewReader(`{"username":"admin","password":"correct horse"}`))
	tlsReq.Header.Set("Content-Type", "application/json")
	tlsResp, err := tlsClient.Do(tlsReq)
	if err != nil {
		t.Fatal(err)
	}
	tlsResp.Body.Close()
	if !strings.Contains(tlsResp.Header.Get("Set-Cookie"), "; Secure") {
		t.Fatalf("direct TLS cookie missing Secure: %q", tlsResp.Header.Get("Set-Cookie"))
	}
}

func TestSpoofedForwardedProtoFromUntrustedPeerDoesNotSetSecureCookie(t *testing.T) {
	trusted := netip.MustParsePrefix("172.18.0.0/16")
	app, _ := newSecurityTestServer(t, config.Config{AdminUsername: "admin", AdminPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080", TrustedProxy: trusted})
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	req, _ := http.NewRequest(http.MethodPost, router.URL+"/api/admin/session", strings.NewReader(`{"username":"admin","password":"correct horse"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := router.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if strings.Contains(resp.Header.Get("Set-Cookie"), "; Secure") {
		t.Fatalf("untrusted forwarded proto unexpectedly marked cookie Secure: %q", resp.Header.Get("Set-Cookie"))
	}
}
