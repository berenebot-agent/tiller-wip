package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
	buildversion "github.com/tiller-router/tiller-router/internal/version"
	webassets "github.com/tiller-router/tiller-router/internal/web"
)

const sessionCookie = "tiller_admin_session"

type Server struct {
	config        config.Config
	db            *database.DB
	clients       *auth.ClientAuthenticator
	sessions      *auth.SessionStore
	providers     *providers.Manager
	oauthFlows    *oauth.FlowStore
	oauthDeviceMu sync.Mutex
	oauthDevices  map[string]*oauthDeviceState
	logger        *slog.Logger
	assets        http.Handler
	// notifyClient is a dedicated HTTP client for best-effort outbound webhook
	// notifications. It has a short timeout so a slow webhook can never
	// materially delay an inference request.
	notifyClient *http.Client
	// notifyCooldownMu guards notifyLastSent, the in-memory last-sent timestamps
	// used to throttle repeat notifications for the same event + model and the
	// in-flight reservations used to prevent concurrent fanout.
	notifyCooldownMu sync.Mutex
	notifyLastSent   map[string]time.Time
	notifyInFlight   map[string]bool
	// loginLimiter throttles failed admin login attempts to blunt brute force.
	loginLimiter         *loginLimiter
	oauthStartLimiter    *loginLimiter
	oauthCallbackLimiter *loginLimiter
	backgroundCtx        context.Context
	// lastOutcome holds the most recent request outcome per real model, keyed
	// by "provider_name/upstream_model_id". It lives in RAM (never persisted) so
	// it is cleared on restart. Written on each routed request; read by the
	// admin usage endpoint to drive the per-target resolution dots.
	lastOutcomeMu sync.RWMutex
	lastOutcome   map[string]lastOutcome
	// live is the SSE hub that pushes outcome deltas and usage snapshots to
	// connected admin tabs. Lazily started on first subscriber, stopped at zero.
	liveHub  *liveHub
	inflight *inflightTracker
}

// lastOutcome is the most recent request result for a single real model.
type lastOutcome struct {
	At        string `json:"at"`         // RFC3339Nano timestamp; empty = never
	Status    int    `json:"status"`     // HTTP status of the last request
	IsSuccess bool   `json:"is_success"` // whether that status was 2xx
}

type contextKey string

const (
	adminSessionKey contextKey = "admin-session"
	clientKey       contextKey = "client"
)

func New(cfg config.Config, db *database.DB, logger *slog.Logger) (*Server, error) {
	clients, err := auth.NewClientAuthenticator(db.SQL)
	if err != nil {
		return nil, err
	}
	sessions, err := auth.NewSessionStore(db.SQL, cfg.AdminUsername, cfg.AdminPassword, cfg.AdminSessionTTL)
	if err != nil {
		return nil, err
	}
	registry := providers.NewRegistry()
	if t, err := db.GetFallbackTimeout(context.Background()); err == nil {
		registry.SetResponseHeaderTimeout(time.Duration(t) * time.Second)
	}
	if cfg.ModelsDevEnabled {
		registry.LoadModelsDevCache(filepath.Join(cfg.DataDir, providers.ModelsDevCacheFile()))
	}
	s := &Server{config: cfg, db: db, clients: clients, sessions: sessions, providers: providers.NewManager(db.SQL, registry), oauthFlows: oauth.NewPersistentFlowStore(nil, filepath.Join(cfg.DataDir, "oauth-flows.json")), oauthDevices: map[string]*oauthDeviceState{}, logger: logger, assets: webassets.Handler(), notifyClient: &http.Client{Timeout: notificationTimeout}, notifyLastSent: map[string]time.Time{}, notifyInFlight: map[string]bool{}, loginLimiter: newLoginLimiter(5, 15*time.Minute, 15*time.Minute), oauthStartLimiter: newLoginLimiter(10, time.Minute, time.Minute), oauthCallbackLimiter: newLoginLimiter(10, time.Minute, time.Minute), backgroundCtx: context.Background(), lastOutcome: map[string]lastOutcome{}, liveHub: &liveHub{outcomeCh: make(chan map[string]lastOutcome, liveOutcomeBuffer), activityCh: make(chan inflightDelta, liveOutcomeBuffer)}, inflight: &inflightTracker{states: map[string]inflightState{}, clientStates: map[string]inflightState{}, targetStates: map[string]inflightState{}}}
	s.inflight.emit = s.liveHub.emitActivity
	s.liveHub.snapshot = s.buildUsageSnapshot
	return s, nil
}
func (s *Server) StartBackground(ctx context.Context) {
	s.backgroundCtx = ctx
	s.providers.StartScheduler(ctx)
	if s.config.ModelsDevEnabled {
		s.providers.Registry().StartModelsDevRefresh(ctx, filepath.Join(s.config.DataDir, providers.ModelsDevCacheFile()))
	}
	go s.startLogPruner(ctx)
}

func (s *Server) startLogPruner(ctx context.Context) {
	s.pruneRequestLogs(ctx) // run once at startup
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pruneRequestLogs(ctx)
		}
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", s.liveHealth)
	mux.HandleFunc("GET /health/ready", s.ready)
	mux.HandleFunc("GET /health/version", s.versionHealth)
	mux.HandleFunc("POST /api/admin/session", s.login)
	mux.Handle("GET /api/admin/session", s.requireAdmin(http.HandlerFunc(s.sessionStatus)))
	mux.Handle("DELETE /api/admin/session", s.requireAdmin(http.HandlerFunc(s.logout)))
	mux.Handle("GET /api/admin/provider-types", s.requireAdmin(http.HandlerFunc(s.providerTypes)))
	mux.Handle("GET /api/admin/providers", s.requireAdmin(http.HandlerFunc(s.listProviders)))
	mux.Handle("POST /api/admin/providers", s.requireAdmin(http.HandlerFunc(s.createProvider)))
	mux.Handle("PATCH /api/admin/providers/{id}", s.requireAdmin(http.HandlerFunc(s.updateProvider)))
	mux.Handle("DELETE /api/admin/providers/{id}", s.requireAdmin(http.HandlerFunc(s.deleteProvider)))
	mux.Handle("PUT /api/admin/providers/{id}/credential", s.requireAdmin(http.HandlerFunc(s.replaceProviderCredential)))
	mux.Handle("POST /api/admin/providers/{id}/oauth/start", s.requireAdmin(http.HandlerFunc(s.startProviderOAuth)))
	mux.Handle("POST /api/admin/providers/{id}/oauth/callback", s.requireAdmin(http.HandlerFunc(s.completeProviderOAuth)))
	mux.Handle("GET /api/admin/providers/{id}/oauth/status", s.requireAdmin(http.HandlerFunc(s.providerOAuthStatus)))
	mux.Handle("DELETE /api/admin/providers/{id}/oauth", s.requireAdmin(http.HandlerFunc(s.disconnectProviderOAuth)))

	mux.Handle("POST /api/admin/providers/{id}/refresh", s.requireAdmin(http.HandlerFunc(s.refreshProvider)))
	mux.Handle("GET /api/admin/providers/{id}/models", s.requireAdmin(http.HandlerFunc(s.listProviderModels)))
	mux.Handle("GET /api/admin/models", s.requireAdmin(http.HandlerFunc(s.listAllModels)))
	mux.Handle("GET /api/admin/virtual-groups", s.requireAdmin(http.HandlerFunc(s.listVirtualGroups)))
	mux.Handle("POST /api/admin/virtual-groups", s.requireAdmin(http.HandlerFunc(s.createVirtualGroup)))
	mux.Handle("PATCH /api/admin/virtual-groups/{id}", s.requireAdmin(http.HandlerFunc(s.updateVirtualGroup)))
	mux.Handle("DELETE /api/admin/virtual-groups/{id}", s.requireAdmin(http.HandlerFunc(s.deleteVirtualGroup)))
	mux.Handle("GET /api/admin/virtual-models", s.requireAdmin(http.HandlerFunc(s.listVirtualModels)))
	mux.Handle("POST /api/admin/virtual-models", s.requireAdmin(http.HandlerFunc(s.createVirtualModel)))
	mux.Handle("PATCH /api/admin/virtual-models/{id}", s.requireAdmin(http.HandlerFunc(s.updateVirtualModel)))
	mux.Handle("DELETE /api/admin/virtual-models/{id}", s.requireAdmin(http.HandlerFunc(s.deleteVirtualModel)))
	mux.Handle("GET /api/admin/client-keys", s.requireAdmin(http.HandlerFunc(s.listClientKeys)))
	mux.Handle("POST /api/admin/client-keys", s.requireAdmin(http.HandlerFunc(s.createClientKey)))
	mux.Handle("PATCH /api/admin/client-keys/{id}", s.requireAdmin(http.HandlerFunc(s.updateClientKey)))
	mux.Handle("DELETE /api/admin/client-keys/{id}", s.requireAdmin(http.HandlerFunc(s.deleteClientKey)))
	mux.Handle("POST /api/admin/client-keys/{id}/rotate", s.requireAdmin(http.HandlerFunc(s.rotateClientKey)))
	mux.Handle("GET /api/admin/client-keys/{id}/permissions", s.requireAdmin(http.HandlerFunc(s.getPermissions)))
	mux.Handle("PUT /api/admin/client-keys/{id}/permissions", s.requireAdmin(http.HandlerFunc(s.updatePermissions)))
	mux.Handle("GET /api/admin/client-keys/{id}/activity", s.requireAdmin(http.HandlerFunc(s.listActivity)))
	mux.Handle("GET /api/admin/client-keys/{id}/activity/export", s.requireAdmin(http.HandlerFunc(s.exportClientActivityCSV)))
	mux.Handle("DELETE /api/admin/client-keys/{id}/activity", s.requireAdmin(http.HandlerFunc(s.clearActivity)))
	mux.Handle("GET /api/admin/virtual-models/{id}/activity", s.requireAdmin(http.HandlerFunc(s.listVirtualActivity)))
	mux.Handle("GET /api/admin/virtual-models/{id}/activity/export", s.requireAdmin(http.HandlerFunc(s.exportVirtualActivityCSV)))
	mux.Handle("GET /api/admin/models/{id}/activity", s.requireAdmin(http.HandlerFunc(s.listRealModelActivity)))
	mux.Handle("GET /api/admin/models/{id}/activity/export", s.requireAdmin(http.HandlerFunc(s.exportRealModelActivityCSV)))
	mux.Handle("GET /api/admin/settings", s.requireAdmin(http.HandlerFunc(s.getSettings)))
	mux.Handle("PUT /api/admin/settings", s.requireAdmin(http.HandlerFunc(s.updateSettings)))
	mux.Handle("POST /api/admin/notifications/test", s.requireAdmin(http.HandlerFunc(s.sendTestNotification)))
	mux.Handle("GET /api/admin/usage", s.requireAdmin(http.HandlerFunc(s.usage)))
	mux.Handle("GET /api/admin/live", s.requireAdmin(http.HandlerFunc(s.live)))
	mux.Handle("GET /api/admin/activity", s.requireAdmin(http.HandlerFunc(s.listGlobalActivity)))
	mux.Handle("GET /api/admin/activity/{id}/attempts", s.requireAdmin(http.HandlerFunc(s.listRequestAttempts)))
	mux.Handle("GET /api/admin/health", s.requireAdmin(http.HandlerFunc(s.adminHealth)))
	mux.Handle("GET /api/admin/backup/export", s.requireAdmin(http.HandlerFunc(s.exportBackup)))
	mux.Handle("GET /v1/models", s.requireClient(http.HandlerFunc(s.clientModels), false))
	mux.Handle("GET /v1/models/{model...}", s.requireClient(http.HandlerFunc(s.clientModel), false))
	mux.Handle("POST /v1/chat/completions", s.requireClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, providers.ProtocolChat) }), false))
	mux.Handle("POST /v1/responses", s.requireClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, providers.ProtocolResponses) }), false))
	mux.Handle("POST /v1/messages", s.requireClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.proxy(w, r, providers.ProtocolMessages) }), true))
	mux.Handle("/", s.assets)
	return s.securityHeaders(s.requestLog(mux))
}

func (s *Server) liveHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "live"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ready(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// versionHealth is deliberately public and contains only non-sensitive build
// metadata, like the other health endpoints.
func (s *Server) versionHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": buildversion.Version, "commit": buildversion.Commit})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	key := clientIP(r, s.config.TrustedProxy)
	if s.loginLimiter.locked(key) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many failed login attempts. Try again later.")
		return
	}
	var input struct{ Username, Password string }
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !auth.EqualCredential(input.Username, s.config.AdminUsername) || !auth.EqualCredential(input.Password, s.config.AdminPassword) {
		if s.loginLimiter.recordFailure(key) {
			adminError(w, http.StatusTooManyRequests, "rate_limited", "Too many failed login attempts. Try again later.")
			return
		}
		adminError(w, http.StatusUnauthorized, "invalid_credentials", "Invalid administrator credentials.")
		return
	}
	s.loginLimiter.success(key)
	session, err := s.sessions.Create()
	if err != nil {
		adminError(w, 500, "internal_error", "Could not create session.")
		return
	}
	s.setSessionCookie(w, r, session.Token, session.ExpiresAt)
	s.notifyAdminEvent(eventAdminLogin, fmt.Sprintf("User: %s\nIP: %s", s.config.AdminUsername, clientIP(r, s.config.TrustedProxy)))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": s.config.AdminUsername, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt.UTC()})
}

// setSessionCookie writes the admin session cookie. Refreshing it on every
// authenticated request keeps the browser cookie's MaxAge in sync with the
// server-side sliding expiry, so active use extends the session across browser
// reopen rather than the cookie expiring 30 days after login.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secureRequest(r), SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
}

func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(adminSessionKey).(auth.Session)
	writeJSON(w, 200, map[string]any{"authenticated": true, "username": s.config.AdminUsername, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt.UTC()})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			adminError(w, 401, "unauthorized", "Administrator authentication required.")
			return
		}
		session, ok := s.sessions.Get(cookie.Value)
		if !ok {
			adminError(w, 401, "unauthorized", "Administrator authentication required.")
			return
		}
		// Refresh the cookie so the browser tracks the sliding session expiry.
		s.setSessionCookie(w, r, cookie.Value, session.ExpiresAt)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sessions.CheckCSRF(session, r.Header.Get("X-CSRF-Token")) {
			adminError(w, 403, "csrf_failed", "A valid CSRF token is required.")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminSessionKey, session)))
	})
}

func (s *Server) requireClient(next http.Handler, anthropic bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r.Header.Get("Authorization"))
		if raw == "" && anthropic {
			raw = r.Header.Get("x-api-key")
		}
		identity, ok := s.clients.Authenticate(raw)
		if !ok {
			inferenceError(w, 401, "authentication_error", "invalid_api_key", "Invalid API key.", anthropic)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey, identity)))
	})
}

// secureRequest reports whether the request arrived over a secure channel so
// the admin session cookie can be marked Secure. A direct TLS connection is
// always secure. When behind a reverse proxy, X-Forwarded-Proto is only trusted
// if the direct peer is within the configured trusted-proxy CIDR, so a client
// cannot force Secure-cookie semantics on a plaintext connection by spoofing the
// header.
func (s *Server) secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !s.config.TrustedProxy.IsValid() {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !s.config.TrustedProxy.Contains(addr) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}

// requestClientIP returns the client address suitable for forwarding to an
// anonymous provider. Forwarded headers are accepted only from the configured
// trusted proxy; otherwise the direct peer address is used. When the direct
// peer is trusted, the authoritative X-Real-IP header is preferred, and
// X-Forwarded-For is only consulted with explicit chain semantics (walking
// right-to-left and removing trusted hops) so a spoofable leftmost value can
// never be trusted.
func (s *Server) requestClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if host == "" {
		return ""
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !s.config.TrustedProxy.IsValid() || !s.config.TrustedProxy.Contains(peer) {
		return host
	}
	if value := strings.TrimSpace(r.Header.Get("X-Real-IP")); value != "" {
		if address, err := netip.ParseAddr(value); err == nil {
			return address.String()
		}
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(parts) == 1 && strings.TrimSpace(parts[0]) == "" {
		return host
	}
	addrs := make([]netip.Addr, len(parts))
	for i, part := range parts {
		addr, parseErr := netip.ParseAddr(strings.TrimSpace(part))
		if parseErr != nil {
			return host
		}
		addrs[i] = addr
	}
	for i := len(addrs) - 1; i >= 0; i-- {
		if !s.config.TrustedProxy.Contains(addrs[i]) {
			return addrs[i].String()
		}
	}
	return addrs[0].String()
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/health/") {
			return
		}
		s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(start).Milliseconds())
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("request body must be valid JSON")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func adminError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}
func inferenceError(w http.ResponseWriter, status int, errType, code, message string, anthropic bool) {
	if anthropic {
		writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": message, "code": code}})
		return
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": errType, "code": code, "message": message}})
}
func bearerToken(header string) string {
	parts := strings.Fields(header)
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return ""
}

func pagination(r *http.Request) (limit, offset int, search string) {
	limit = 50
	if _, err := fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit); err != nil || limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	_, _ = fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
	if offset < 0 {
		offset = 0
	}
	search = strings.TrimSpace(r.URL.Query().Get("search"))
	return
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func scanBool(v int) bool { return v != 0 }

func (s *Server) exportBackup(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.config.DataDir, "backups")
	path, err := s.db.Backup(r.Context(), dir)
	if err != nil {
		adminError(w, 500, "backup_failed", "Could not create a consistent backup.")
		return
	}
	defer os.Remove(path)
	w.Header().Set("Content-Type", mime.TypeByExtension(".db"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Tiller-Secret-Material", "provider-credentials")
	http.ServeFile(w, r, path)
}
