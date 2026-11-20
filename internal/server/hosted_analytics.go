package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
)

func (s *Server) hostedAnalyticsEnabled() bool {
	if s.config.Mode != config.ModeHosted {
		return false
	}
	s.platformAnalyticsMu.RLock()
	defer s.platformAnalyticsMu.RUnlock()
	return s.platformAnalytics.TotalsEnabled
}

func (s *Server) countHostedSignup(ctx context.Context) {
	if s.hostedAnalyticsEnabled() {
		s.incrementHostedAnalytics(ctx, "signups")
	}
}

func (s *Server) incrementHostedAnalytics(ctx context.Context, metric string) {
	// Counting is best effort and has a short deadline so a slow database
	// cannot stall a page load or change the outcome of a signup.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
	defer cancel()
	_ = s.storeHandle().IncrementHostedAnalytics(ctx, time.Now(), metric)
}

func (s *Server) countHostedPage(w http.ResponseWriter, r *http.Request) {
	var metric string
	switch r.URL.Path {
	case "/", "/login":
		metric = "app_loads"
	case "/legal/privacy":
		metric = "privacy_loads"
	case "/legal/terms":
		metric = "terms_loads"
	}
	if metric == "" || r.Method != http.MethodGet || !s.hostedAnalyticsEnabled() {
		s.assets.ServeHTTP(w, r)
		return
	}
	response := &hostedPageResponse{ResponseWriter: w, status: http.StatusOK}
	s.assets.ServeHTTP(response, r)
	if response.status == http.StatusOK && strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		s.incrementHostedAnalytics(r.Context(), metric)
	}
}

type hostedPageResponse struct {
	http.ResponseWriter
	status int
}

func (w *hostedPageResponse) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
