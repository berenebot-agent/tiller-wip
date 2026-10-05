package web

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

//go:embed assets/*
var files embed.FS

// reservedRoutePrefixes are application-owned routes that a custom landing site
// must never serve. They cover the API, health checks, and the SPA entry routes
// (including /app, which would otherwise swallow the /app.js bundle path check
// below).
var reservedRoutePrefixes = []string{"/api", "/health", "/app", "/login", "/signup", "/platform"}

// embeddedRootFiles and embeddedRootDirs are the top-level entries of the
// embedded application asset tree. A custom site may not shadow any of them:
// the SPA entry bundle (app.js) and its siblings (live.js, activity-graph.js,
// d3.min.js, the CSS files) are loaded by the embedded index.html at fixed
// paths, so a custom asset at the same path would break the application UI.
//
// index.html is deliberately excluded: the custom landing page is served at
// "/" and "/index.html" and owns that file.
var embeddedRootFiles, embeddedRootDirs = embeddedAssetRoots()

func embeddedAssetRoots() (map[string]struct{}, map[string]struct{}) {
	filesSet := map[string]struct{}{}
	dirsSet := map[string]struct{}{}
	entries, err := fs.ReadDir(files, "assets")
	if err != nil {
		return filesSet, dirsSet
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "index.html" {
			continue
		}
		if entry.IsDir() {
			dirsSet[name] = struct{}{}
			continue
		}
		filesSet[name] = struct{}{}
	}
	return filesSet, dirsSet
}

func isReservedPath(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// appOwnedAsset reports whether a URL path is served by the embedded
// application and therefore cannot be provided by a custom site.
func appOwnedAsset(p string) bool {
	rel := strings.TrimPrefix(p, "/")
	if rel == "" {
		return false
	}
	top := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		top = rel[:i]
	}
	if _, ok := embeddedRootFiles[top]; ok {
		return true
	}
	_, ok := embeddedRootDirs[top]
	return ok
}

// reservedCustomPath reports whether a request path is application-owned, by
// route prefix or by embedded asset, and must not be served from a custom site.
func reservedCustomPath(p string) bool {
	for _, prefix := range reservedRoutePrefixes {
		if isReservedPath(p, prefix) {
			return true
		}
	}
	return appOwnedAsset(p)
}

func Handler() http.Handler {
	handler, _ := HandlerWithSite("", nil)
	return handler
}

// HandlerWithSite serves the embedded application and, when siteDir is set,
// an operator-supplied landing page from that directory. The external site is
// intentionally limited to the public asset surface; reserved API/health and
// application routes, plus the embedded application asset files, continue to
// resolve to the embedded application. A custom file that would shadow a
// reserved path is ignored and reported once via logger.
func HandlerWithSite(siteDir string, logger *slog.Logger) (http.Handler, error) {
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
	var warnMu sync.Mutex
	warned := map[string]bool{}
	warnShadowed := func(p string) {
		if logger == nil {
			return
		}
		warnMu.Lock()
		if warned[p] {
			warnMu.Unlock()
			return
		}
		warned[p] = true
		warnMu.Unlock()
		logger.Warn("custom site asset is shadowed by a reserved application path and will not be served", "path", p)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if siteDir != "" && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.ServeFile(w, r, filepath.Join(siteDir, "index.html"))
			return
		}
		if siteDir != "" && serveCustomAsset(w, r, siteDir, warnShadowed) {
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

func serveCustomAsset(w http.ResponseWriter, r *http.Request, siteDir string, warnShadowed func(string)) bool {
	rel := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	fullPath := filepath.Join(siteDir, filepath.FromSlash(rel))
	info, err := os.Stat(fullPath)
	exists := err == nil && info.Mode().IsRegular()
	if reservedCustomPath(r.URL.Path) {
		if exists {
			warnShadowed(r.URL.Path)
		}
		return false
	}
	if !exists {
		return false
	}
	if strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
		w.Header().Set("Cache-Control", "no-store")
	}
	http.ServeFile(w, r, fullPath)
	return true
}

// ShadowedCustomAssets returns the custom-site files whose paths are
// application-owned and therefore ignored when serving. The paths are
// slash-prefixed and sorted. The root index.html is not reported: the custom
// landing page owns it.
func ShadowedCustomAssets(siteDir string) ([]string, error) {
	if siteDir == "" {
		return nil, nil
	}
	var shadowed []string
	err := filepath.WalkDir(siteDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(siteDir, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "index.html" {
			return nil
		}
		if reservedCustomPath("/" + rel) {
			shadowed = append(shadowed, "/"+rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(shadowed)
	return shadowed, nil
}
