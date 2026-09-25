package web

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed assets/*
var files embed.FS

func isReservedPath(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func Handler() http.Handler {
	handler, _ := HandlerWithSite("")
	return handler
}

// HandlerWithSite serves the embedded application and, when siteDir is set,
// an operator-supplied landing page from that directory. The external site is
// intentionally limited to the public asset surface; reserved API/health and
// application routes continue to resolve to the embedded application/router.
func HandlerWithSite(siteDir string) (http.Handler, error) {
	sub, _ := fs.Sub(files, "assets")
	indexHTML, _ := fs.ReadFile(sub, "index.html")
	if siteDir != "" {
		info, err := os.Stat(filepath.Join(siteDir, "index.html"))
		if err != nil {
			return nil, fmt.Errorf("custom site index: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("custom site index is not a regular file: %s", filepath.Join(siteDir, "index.html"))
		}
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if siteDir != "" && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeFile(w, r, filepath.Join(siteDir, "index.html"))
			return
		}
		if siteDir != "" && serveCustomAsset(w, r, siteDir) {
			return
		}

		// The admin UI is embedded into the binary and changes whenever the
		// router is rebuilt. Prevent browsers from retaining an older bundle,
		// which can otherwise make the editor appear out of sync with the API.
		isEntry := r.URL.Path == "/" || r.URL.Path == "/index.html" ||
			(!isReservedPath(r.URL.Path, "/api") && !isReservedPath(r.URL.Path, "/health") && !strings.Contains(r.URL.Path, "."))
		if isEntry || strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if isEntry {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(indexHTML)
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}

func serveCustomAsset(w http.ResponseWriter, r *http.Request, siteDir string) bool {
	for _, prefix := range []string{"/api", "/health", "/app", "/login", "/signup", "/platform"} {
		if isReservedPath(r.URL.Path, prefix) {
			return false
		}
	}
	rel := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	fullPath := filepath.Join(siteDir, filepath.FromSlash(rel))
	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
		w.Header().Set("Cache-Control", "no-store")
	}
	http.ServeFile(w, r, fullPath)
	return true
}
