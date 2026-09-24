package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets/*
var files embed.FS

func isReservedPath(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func Handler() http.Handler {
	sub, _ := fs.Sub(files, "assets")
	indexHTML, _ := fs.ReadFile(sub, "index.html")
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
}
