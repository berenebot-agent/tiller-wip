package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

func platformTestServer(t *testing.T, platformPort int) *Server {
	t.Helper()
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(config.Config{
		Mode: config.ModeHosted, TillerUser: "owner@example.com", TillerUserPassword: "correct horse battery staple",
		TillerPlatformAdminUser: "platform-admin", TillerPlatformAdminPassword: "platform-secret-0123456789",
		PublicURL: "https://tiller.example.com", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, DataDir: t.TempDir(),
		PlatformPort: platformPort,
	}, db, slog.New(slog.NewTextHandler(io.Discard, nil)), withSecretHasher(fastsecret.Hasher{}))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func platformLoginRequest(t *testing.T, client *http.Client, base, origin string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", base+"/api/platform/session", strings.NewReader(`{"username":"platform-admin","password":"platform-secret-0123456789"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp, payload
}

func TestPlatformConsoleOnMainPortWhenPortUnset(t *testing.T) {
	app := platformTestServer(t, 0)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	resp, payload := platformLoginRequest(t, &http.Client{Jar: jar}, srv.URL, "")
	if resp.StatusCode != 200 {
		t.Fatalf("platform login on main port = %d %v, want 200", resp.StatusCode, payload)
	}
	if exposed, _ := payload["platform_exposed"].(bool); !exposed {
		t.Fatalf("platform_exposed = %v, want true when the console shares the public port", payload["platform_exposed"])
	}
}

func TestPlatformConsoleMovesToDedicatedListener(t *testing.T) {
	app := platformTestServer(t, 9443)
	public := httptest.NewServer(app.Handler())
	private := httptest.NewServer(app.PlatformHandler())
	t.Cleanup(public.Close)
	t.Cleanup(private.Close)

	// The main listener no longer knows the console at all.
	for _, path := range []string{"/platform", "/platform/", "/api/platform/session", "/api/platform/stats"} {
		resp, err := http.Get(public.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Errorf("public GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	if resp, _ := platformLoginRequest(t, http.DefaultClient, public.URL, ""); resp.StatusCode != 404 {
		t.Fatalf("public platform login = %d, want 404", resp.StatusCode)
	}
	// Customer routes are unaffected on the main listener.
	if resp, err := http.Get(public.URL + "/api/auth/options"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("public /api/auth/options broke: %v %v", resp, err)
	}

	// The dedicated listener serves the console, but not customer routes.
	if resp, err := http.Get(private.URL + "/api/auth/options"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("platform listener served a customer route: %v %v", resp, err)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	// An Origin that is the public site (not the address used) is cross-site here.
	if resp, _ := platformLoginRequest(t, client, private.URL, "https://tiller.example.com"); resp.StatusCode != 400 {
		t.Fatalf("login with foreign Origin = %d, want 400", resp.StatusCode)
	}
	resp, payload := platformLoginRequest(t, client, private.URL, private.URL)
	if resp.StatusCode != 200 {
		t.Fatalf("platform login on dedicated listener = %d %v", resp.StatusCode, payload)
	}
	if exposed, _ := payload["platform_exposed"].(bool); exposed {
		t.Fatal("platform_exposed = true on the dedicated listener")
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		cookie = c
	}
	if cookie == nil || cookie.Name != platformInsecureSessionCookie || cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("plain-HTTP platform cookie = %+v, want HttpOnly SameSite=Strict %s without Secure", cookie, platformInsecureSessionCookie)
	}
	statusResp, err := client.Get(private.URL + "/api/platform/session")
	if err != nil {
		t.Fatal(err)
	}
	statusResp.Body.Close()
	if statusResp.StatusCode != 200 {
		t.Fatalf("session status after login = %d, want 200", statusResp.StatusCode)
	}
	// The insecure cookie name is not honoured by the public handler's rules.
	if cookie.Name == platformSessionCookie {
		t.Fatal("dedicated listener over HTTP must not use the __Host- cookie")
	}
}
