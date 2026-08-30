package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets/*
var files embed.FS

func Handler() http.Handler {
	sub, _ := fs.Sub(files, "assets")
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The admin UI is embedded into the binary and changes whenever the
		// router is rebuilt. Prevent browsers from retaining an older bundle,
		// which can otherwise make the editor appear out of sync with the API.
		if r.URL.Path == "/" || r.URL.Path == "/index.html" ||
			strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Cache-Control", "no-store")
		}
		fileServer.ServeHTTP(w, r)
	})
}
