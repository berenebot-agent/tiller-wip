package server

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
)

// TestDebugPprofDisabledByDefault verifies the profiling surface is absent when
// TILLER_DEBUG_PPROF has not been enabled. The catch-all asset handler answers
// the unmatched path, so the route must resolve to 404 (not 200/401).
func TestDebugPprofDisabledByDefault(t *testing.T) {
	app, _ := newSecurityTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"})
	req := httptest.NewRequest(http.MethodGet, debugPprofPrefix+"heap", nil)
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusNotFound {
		t.Fatalf("pprof route with DebugPprof=false: status = %d, want 404", response.Code)
	}
}

// TestDebugEndpointsRequireAdmin verifies both debug endpoints reject an
// unauthenticated request when profiling is enabled.
func TestDebugEndpointsRequireAdmin(t *testing.T) {
	app, _ := newSecurityTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080", DebugPprof: true})
	for _, path := range []string{"/api/admin/debug/memory", debugPprofPrefix + "heap", debugPprofPrefix} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, req)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", path, response.Code)
		}
	}
}

func TestDebugEndpointsRequirePlatformAdminInHostedMode(t *testing.T) {
	app, _, _, _ := legalTestServer(t)
	app.config.DebugPprof = true
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: server.URL, client: &http.Client{Jar: jar}, server: app}
	status, payload, _ := api.request(http.MethodPost, "/api/platform/session", map[string]any{"username": "platform-admin", "password": "platform-secret"})
	if status != http.StatusOK {
		t.Fatalf("platform login: %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)

	for _, path := range []string{"/api/admin/debug/memory", debugPprofPrefix + "heap", debugPprofPrefix} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", path, resp.StatusCode)
		}
	}

	for _, path := range []string{"/api/admin/debug/memory", debugPprofPrefix + "goroutine?debug=1"} {
		status, _, _ := api.request(http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("authenticated %s: status = %d, want 200", path, status)
		}
	}
}

// endpoint returns a runtime summary and the pprof handler proxies through to
// the standard handlers, both behind the admin gate.
func TestDebugEndpointsServeWhenEnabledAndAuthenticated(t *testing.T) {
	app, _ := newSecurityTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080", DebugPprof: true})
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	login, err := router.Client().Post(router.URL+"/api/admin/session", "application/json",
		strings.NewReader(`{"username":"admin","password":"correct horse"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", login.StatusCode)
	}
	cookie := strings.SplitN(strings.SplitN(login.Header.Get("Set-Cookie"), ";", 2)[0], "=", 2)[1]

	get := func(path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, router.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cookie", sessionCookie+"="+cookie)
		resp, err := router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	memResp := get("/api/admin/debug/memory")
	defer memResp.Body.Close()
	if memResp.StatusCode != http.StatusOK {
		t.Fatalf("memory endpoint: %d", memResp.StatusCode)
	}
	var memory map[string]any
	if err := json.NewDecoder(memResp.Body).Decode(&memory); err != nil {
		t.Fatalf("decode memory endpoint: %v", err)
	}
	if _, ok := memory["heap_alloc"]; !ok {
		t.Fatalf("memory payload missing heap_alloc: %v", memory)
	}
	if _, ok := memory["goroutines"]; !ok {
		t.Fatalf("memory payload missing goroutines: %v", memory)
	}

	pprofResp := get(debugPprofPrefix + "goroutine?debug=1")
	defer pprofResp.Body.Close()
	if pprofResp.StatusCode != http.StatusOK {
		t.Fatalf("pprof endpoint: %d", pprofResp.StatusCode)
	}
}
