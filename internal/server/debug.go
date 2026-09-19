package server

import (
	"net/http"
	"os"
	"runtime"
	"strings"

	// Registers the standard pprof handlers on http.DefaultServeMux. This mux
	// is never served directly; debugPprof below is the only entry point and it
	// sits behind s.requireAdmin when TILLER_DEBUG_PPROF is enabled.
	_ "net/http/pprof"
)

const debugPprofPrefix = "/api/admin/debug/pprof/"

// debugMemory reports a bounded Go runtime memory summary plus the configured
// soft-limit knobs. It is admin-gated and read-only. There is no non-mutating
// getter for the live GOMEMLIMIT value (debug.SetMemoryLimit returns the
// previous setting while applying the new one), so the configured environment
// values are reported verbatim instead.
func (s *Server) debugMemory(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, http.StatusOK, map[string]any{
		"goroutines":        runtime.NumGoroutine(),
		"gomemlimit_env":    envOrUnset("GOMEMLIMIT"),
		"gogc_env":          envOrUnset("GOGC"),
		"sys":               m.Sys,
		"heap_alloc":        m.HeapAlloc,
		"heap_sys":          m.HeapSys,
		"heap_idle":         m.HeapIdle,
		"heap_inuse":        m.HeapInuse,
		"heap_released":     m.HeapReleased,
		"heap_objects":      m.HeapObjects,
		"stack_inuse":       m.StackInuse,
		"stack_sys":         m.StackSys,
		"mspan_inuse":       m.MSpanInuse,
		"mcache_inuse":      m.MCacheInuse,
		"gc_sys":            m.GCSys,
		"other_sys":         m.OtherSys,
		"num_gc":            m.NumGC,
		"last_gc_unix_nano": m.LastGC,
		"pause_total_ns":    m.PauseTotalNs,
		"pause_ns":          m.PauseNs[(m.NumGC+255)%256],
	})
}

// debugPprof exposes the standard net/http/pprof handlers under the admin
// prefix. The request path is rewritten back to /debug/pprof/<name> before
// delegating to http.DefaultServeMux, which is where the blank pprof import
// registered them.
func (s *Server) debugPprof(w http.ResponseWriter, r *http.Request) {
	cloned := r.Clone(r.Context())
	cloned.URL.Path = "/debug/pprof/" + strings.TrimPrefix(r.URL.Path, debugPprofPrefix)
	http.DefaultServeMux.ServeHTTP(w, cloned)
}

func envOrUnset(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return "unset"
}
