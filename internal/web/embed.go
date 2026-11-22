package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
		index, err := os.OpenInRoot(siteDir, "index.html")
		if err != nil {
			return nil, fmt.Errorf("custom site index: %w", err)
		}
		info, err := index.Stat()
		index.Close()
		if err != nil {
			return nil, fmt.Errorf("custom site index: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("custom site index is not a regular file: %s", filepath.Join(siteDir, "index.html"))
		}
	}
	fileServer := http.FileServer(http.FS(sub))
	etags := embeddedAssetETags()
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
		if siteDir != "" {
			for _, segment := range strings.Split(r.URL.Path, "/") {
				if segment == ".." {
					http.Error(w, "invalid URL path", http.StatusBadRequest)
					return
				}
			}
			// Use the same canonical path for routing and file access so aliases
			// cannot bypass the application-owned route and asset checks.
			r = r.Clone(r.Context())
			r.URL.Path = path.Clean("/" + r.URL.Path)
			r.URL.RawPath = ""
		}
		if siteDir != "" && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if !serveCustomAsset(w, r, siteDir, warnShadowed) {
				http.NotFound(w, r)
			}
			return
		}
		if siteDir != "" && serveCustomAsset(w, r, siteDir, warnShadowed) {
			return
		}

		// The admin UI is embedded into the binary and changes whenever the
		// router is rebuilt. Prevent browsers from retaining an older bundle,
		// which can otherwise make the editor appear out of sync with the API.
		requestPath := r.URL.Path
		isEntry := requestPath == "/" || requestPath == "/index.html" ||
			requestPath == "/platform" || requestPath == "/platform/" ||
			(!isReservedPath(requestPath, "/api") && !isReservedPath(requestPath, "/health") &&
				!isReservedPath(requestPath, "/platform") && !strings.Contains(requestPath, "."))
		if isEntry || strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if isEntry {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(indexHTML)
			return
		}
		// Embedded assets are content-addressed rather than cacheable: the ETag
		// changes whenever the binary does, so a rebuild can never leave a
		// browser running the previous bundle. no-store alone is not enough —
		// Chromium still reuses a cached response that has no validator, which
		// served a stale app.js against a newer API and only a hard reload
		// cleared. If-None-Match is still honoured so an unchanged asset costs a
		// 304 rather than a re-download.
		if etag, ok := etags[strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")]; ok {
			w.Header().Set("ETag", etag)
			if matchesETag(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}

// embeddedAssetETags maps each embedded asset path to a quoted strong ETag
// derived from its bytes. The content is fixed at build time, so the digest is
// computed once per handler rather than per request.
func embeddedAssetETags() map[string]string {
	sub, err := fs.Sub(files, "assets")
	if err != nil {
		return nil
	}
	etags := map[string]string{}
	_ = fs.WalkDir(sub, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		data, readErr := fs.ReadFile(sub, name)
		if readErr != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		etags[name] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return etags
}

// matchesETag reports whether an If-None-Match header lists the given ETag. A
// literal "*" matches any current representation, per RFC 9110.
func matchesETag(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		if weak, ok := strings.CutPrefix(candidate, "W/"); ok && weak == etag {
			return true
		}
	}
	return false
}

func serveCustomAsset(w http.ResponseWriter, r *http.Request, siteDir string, warnShadowed func(string)) bool {
	rel := strings.TrimPrefix(r.URL.Path, "/")
	if rel == "" {
		rel = "index.html"
	}
	if !fs.ValidPath(rel) {
		return false
	}
	// OpenInRoot (a rooted filesystem primitive) is the traversal boundary:
	// siteDir is opened once and every component of rel is resolved relative
	// to it, so the result cannot escape it — via "..", nested traversal,
	// encoded traversal, backslash/separator variants, absolute paths, or a
	// symlink that points outside siteDir. The pre-check above rejects ".."
	// early; OpenInRoot is the actual enforcement. Stat and serve the same
	// open file to avoid a check-then-open race.
	file, err := os.OpenInRoot(siteDir, rel)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
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
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
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
