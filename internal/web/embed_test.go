package web

import (
	"net/http"
	"net/http/httptest"
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
