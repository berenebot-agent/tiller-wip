package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHandlerServesSPAEntryWithoutRedirect(t *testing.T) {
	tests := []string{"/", "/index.html", "/platform", "/login", "/verify-email?token=x", "/reset-password?token=x"}
	handler := Handler()

	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			res := httptest.NewRecorder()

			handler.ServeHTTP(res, req)

			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
			}
			if location := res.Header().Get("Location"); location != "" {
				t.Fatalf("Location = %q, want empty", location)
			}
			if contentType := res.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
				t.Fatalf("Content-Type = %q, want HTML", contentType)
			}
			if cacheControl := res.Header().Get("Cache-Control"); cacheControl != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", cacheControl)
			}
			if !strings.Contains(res.Body.String(), "<!doctype html>") {
				t.Fatal("response does not contain embedded HTML")
			}
		})
	}
}

func TestHandlerWithSiteUsesExternalLandingAndAssets(t *testing.T) {
	siteDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(siteDir, "index.html"), []byte("<!doctype html><title>Custom</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "landing.css"), []byte("body { color: red; }"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A custom file at an application asset path must be ignored: the embedded
	// bundle always wins so the SPA entry cannot be shadowed.
	if err := os.WriteFile(filepath.Join(siteDir, "style.css"), []byte("body { color: blue; }"), 0o600); err != nil {
		t.Fatal(err)
	}

	handler, err := HandlerWithSite(siteDir, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	landing := request("/")
	if landing.Code != http.StatusOK || !strings.Contains(landing.Body.String(), "Custom") {
		t.Fatalf("custom landing: status=%d body=%q", landing.Code, landing.Body.String())
	}
	asset := request("/landing.css")
	if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "color: red") {
		t.Fatalf("custom asset: status=%d body=%q", asset.Code, asset.Body.String())
	}
	bundle := request("/app.js")
	if bundle.Code != http.StatusOK || !strings.Contains(bundle.Body.String(), "LiveStream") {
		t.Fatalf("application bundle /app.js was replaced by the custom site: status=%d", bundle.Code)
	}
	stylesheet := request("/style.css")
	if stylesheet.Code != http.StatusOK || strings.Contains(stylesheet.Body.String(), "color: blue") {
		t.Fatalf("application stylesheet /style.css was replaced by the custom site: status=%d", stylesheet.Code)
	}
	app := request("/app")
	if app.Code != http.StatusOK || !strings.Contains(app.Body.String(), "<!doctype html>") {
		t.Fatalf("app route was replaced by custom site: status=%d body=%q", app.Code, app.Body.String())
	}
}

func TestHandlerWithSiteWarnsOnceForShadowedAsset(t *testing.T) {
	siteDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(siteDir, "index.html"), []byte("<!doctype html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(siteDir, "app.js"), []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	handler, err := HandlerWithSite(siteDir, logger)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if !strings.Contains(res.Body.String(), "LiveStream") {
			t.Fatalf("request %d served a shadowed custom asset", i)
		}
	}
	if got := strings.Count(buf.String(), "shadowed by a reserved application path"); got != 1 {
		t.Fatalf("warning count = %d, want 1; log=%q", got, buf.String())
	}
}

func TestShadowedCustomAssetsReportsCollisions(t *testing.T) {
	siteDir := t.TempDir()
	files := map[string]string{
		"index.html":      "<!doctype html>",
		"landing.css":     "body {}",
		"style.css":       "body {}",
		"app.js":          "console.log(1)",
		"media/photo.png": "x",
		"nested/live.js":  "x",
	}
	for name, content := range files {
		full := filepath.Join(siteDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shadowed, err := ShadowedCustomAssets(siteDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/app.js", "/media/photo.png", "/style.css"}
	if !reflect.DeepEqual(shadowed, want) {
		t.Fatalf("shadowed = %v, want %v", shadowed, want)
	}
}

func TestHandlerWithSiteRequiresIndex(t *testing.T) {
	if _, err := HandlerWithSite(t.TempDir(), nil); err == nil {
		t.Fatal("custom site without index.html should fail")
	}
}

func TestHandlerServesStaticAssetsAndDoesNotRouteAPIOrHealthToSPA(t *testing.T) {
	handler := Handler()

	tests := []struct {
		name         string
		path         string
		statusCode   int
		contentType  string
		cacheControl string
	}{
		{name: "asset", path: "/style.css", statusCode: http.StatusOK, contentType: "text/css; charset=utf-8", cacheControl: "no-store"},
		{name: "api", path: "/api", statusCode: http.StatusNotFound},
		{name: "api child", path: "/api/missing", statusCode: http.StatusNotFound},
		{name: "health", path: "/health", statusCode: http.StatusNotFound},
		{name: "health child", path: "/health/missing", statusCode: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			res := httptest.NewRecorder()

			handler.ServeHTTP(res, req)

			if res.Code != test.statusCode {
				t.Fatalf("status = %d, want %d", res.Code, test.statusCode)
			}
			if test.contentType != "" && res.Header().Get("Content-Type") != test.contentType {
				t.Fatalf("Content-Type = %q, want %q", res.Header().Get("Content-Type"), test.contentType)
			}
			if test.cacheControl != "" && res.Header().Get("Cache-Control") != test.cacheControl {
				t.Fatalf("Cache-Control = %q, want %q", res.Header().Get("Cache-Control"), test.cacheControl)
			}
			if strings.Contains(res.Body.String(), "<!doctype html>") {
				t.Fatal("API or health response unexpectedly contains SPA HTML")
			}
		})
	}
}
