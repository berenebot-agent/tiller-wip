package server

import (
	"net/http"
	"sync"
)

// bodyCaptureWriter retains only bytes actually written to the client. The
// keepalive timer can write concurrently, so capture and snapshots share a lock.
type bodyCaptureWriter struct {
	http.ResponseWriter
	mu        sync.Mutex
	body      []byte
	truncated bool
}

func (w *bodyCaptureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.ResponseWriter.Write(p)
	remaining := maxLoggedBodyBytes - len(w.body)
	if n > remaining {
		w.truncated = true
		w.body = append(w.body, p[:remaining]...)
	} else {
		w.body = append(w.body, p[:n]...)
	}
	return n, err
}

func (w *bodyCaptureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *bodyCaptureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *bodyCaptureWriter) snapshot() (*string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.body) == 0 {
		return nil, false
	}
	body := string(w.body)
	return &body, w.truncated
}
