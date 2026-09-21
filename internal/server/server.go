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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/hostednet"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/mailer"
	"github.com/tiller-router/tiller-router/internal/mailoutbox"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/providers/oauth"
	"github.com/tiller-router/tiller-router/internal/store"
	buildversion "github.com/tiller-router/tiller-router/internal/version"
	webassets "github.com/tiller-router/tiller-router/internal/web"
)

const (
	sessionCookie         = "tiller_admin_session"
	userSessionCookie     = "__Host-tiller_session"
	platformSessionCookie = "__Host-tiller_platform_session"
)

type Server struct {
	config config.Config
	db     *database.DB
	// store is the single boundary for tenant-table SQL. Handlers obtain an
	// account-scoped handle via s.scope(r) or s.store.For(accountID).
	store *store.Store
	// secretCipher is the resolved recoverable-secret cipher. nil means the
	// passthrough default; it is reported as "disabled" to the admin status
	// API. Locked is reported when the cipher holds no usable key.
	secretCipher store.SecretCipher
	clients      *auth.ClientAuthenticator
	sessions     *auth.SessionStore
	identity     *identity.Store
	mailer       *mailer.Manager
	// outbox is the durable transactional-mail queue. It is nil in local mode,
	// where identity flows never enqueue.
	outbox *mailoutbox.Outbox
	// adminAccount resolves the account owned by an authenticated admin
	// session. Nil means the single implicit local account (Phase 1); Phase 3
	// wires hosted users in here. Tests inject a non-local account.
	adminAccount func(auth.Session) string
	// secretHasher is the token hasher used when generating client keys
	// (bcrypt in production; a fast hasher in tests).
	secretHasher  auth.SecretHasher
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
	loginLimiter          *loginLimiter
	userLoginLimiter      *loginLimiter
	signupLimiter         *loginLimiter
	recoveryLimiter       *loginLimiter
	clientSelectorLimiter *loginLimiter
	clientAddressLimiter  *loginLimiter
	oauthStartLimiter     *loginLimiter
	oauthCallbackLimiter  *loginLimiter
	backgroundCtx         context.Context
	// lastOutcome holds the most recent request outcome per real model, keyed
	// by account + provider_model_id. It lives in RAM (never persisted) so it
	// is cleared on restart. Written on each routed request; read by the admin
	// usage endpoint to drive the per-target resolution dots.
	lastOutcomeMu sync.RWMutex
	lastOutcome   map[string]lastOutcome
	// live is the SSE hub that pushes outcome deltas and usage snapshots to
	// connected admin tabs. Lazily started on first subscriber, stopped at zero.
	liveHub  *liveHub
	inflight *inflightTracker
	// cooldown holds the in-memory per-target fallback cooldown state keyed by
	// account + provider_model_id. It lives in RAM only and is cleared on
	// restart.
	cooldown *cooldownStore
	// usageAggMu guards the cached DB-derived usage aggregates. The live
	// in-memory state (last outcomes, cooldowns, in-flight) is never cached, so
	// it stays fresh even while the expensive request_logs scans are reused.
	usageAggMu sync.Mutex
	usageAgg   map[string]*usageAggregates
	usageAggAt map[string]time.Time
	// usageCacheTTL bounds how long a computed usage aggregate is reused. Zero
	// disables caching (tests use this for determinism).
	usageCacheTTL time.Duration
	// sseKeepalive overrides the streaming keepalive interval. Zero uses the
	// production default (sseKeepaliveInterval); tests set it short so the
	// silence path is exercised without waiting.
	sseKeepalive time.Duration
	// logWriter batch-writes Activity rows off the request path. It is nil
	// until StartBackground runs; while nil, writeLog writes synchronously so
	// tests and direct Server construction stay deterministic.
	logWriter          *logWriter
	platformSettingsMu sync.Mutex
}

// secretEncryptionState reports the credential-encryption state for the admin
// status API: "enabled", "disabled" (no cipher configured; tests/hash-only),
// or "locked" (the master key is missing or does not match the stored
// ciphertext). It never exposes key material, fingerprints, or nonces.
func (s *Server) secretEncryptionState() string {
	return store.SecretsState(s.secretCipher)
}

// secretsLocked reports whether the recoverable-secret cipher is in the locked
// state.
func (s *Server) secretsLocked() bool {
	return s.secretCipher != nil && s.secretCipher.Locked()
}

// keepaliveInterval returns the streaming keepalive cadence, honouring the
// test override when set.
func (s *Server) keepaliveInterval() time.Duration {
	if s.sseKeepalive > 0 {
		return s.sseKeepalive
	}
	return sseKeepaliveInterval
}

// lastOutcome is the most recent request result for a single real model.
type lastOutcome struct {
	At        string `json:"at"`         // RFC3339Nano timestamp; empty = never
	Status    int    `json:"status"`     // HTTP status of the last request
	IsSuccess bool   `json:"is_success"` // whether that status was 2xx
	// Result is the explicit outcome kind: "success", "failed" or "skipped".
	// Empty on legacy entries recorded before this field existed.
	Result string `json:"result,omitempty"`
	// FailureClass names why a failed/skipped attempt did not serve.
	FailureClass string `json:"failure_class,omitempty"`
	// Degrading reports whether this outcome should affect the target's
	// main-page health. A genuine upstream failure is degrading even when a
	// later fallback rescues the request: target health is per-target, while
	// request health is separate. Skipped targets (never called) and
	// client-caused failures are non-degrading. The live graph still sees
	// every attempt's explicit outcome.
	Degrading bool `json:"degrading"`
}

type contextKey string

const (
	adminSessionKey    contextKey = "admin-session"
	userSessionKey     contextKey = "user-session"
	platformSessionKey contextKey = "platform-session"
	userKey            contextKey = "user"
	clientKey          contextKey = "client"
	// accountKey carries the verified AccountID for the request. It is set by
	// the authentication middleware from the authenticated principal only —
	// never from request input.
	accountKey contextKey = "account"
)

type serverOption func(*serverOptions)

type serverOptions struct {
	// tokenHasher hashes high-entropy machine tokens (client API keys, admin
	// session tokens). credentialHasher hashes the low-entropy admin credential
	// fingerprint, which keeps the memory-hard KDF.
	tokenHasher      auth.SecretHasher
	credentialHasher auth.SecretHasher
	passwordHasher   auth.SecretHasher
	// adminAccount overrides the account owned by an admin session. Unexported
	// so only in-package tests can inject a non-local admin principal; Phase 1
	// production always owns the implicit local account.
	adminAccount func(auth.Session) string
	// cipher encrypts recoverable tenant secrets. nil means the passthrough
	// default (tests and hash-only paths). Production injects the resolved
	// master-key cipher via WithSecretCipher.
	cipher store.SecretCipher
}

// WithSecretCipher injects the resolved master-key cipher used to encrypt and
// decrypt provider credentials, OAuth tokens, and secret settings. It is
// exported because the composition root (cmd/tiller-router) resolves the key
// from config and the data directory before constructing the server.
func WithSecretCipher(c store.SecretCipher) serverOption {
	return func(o *serverOptions) {
		if c != nil {
			o.cipher = c
		}
	}
}

// withAdminAccount injects an admin-principal account resolver. Unexported so
// only in-package tests can use it.
func withAdminAccount(fn func(auth.Session) string) serverOption {
	return func(o *serverOptions) {
		o.adminAccount = fn
	}
}

// withSecretHasher sets a single SecretHasher for both token hashing and the
// admin credential fingerprint. Unexported so only in-package tests can use it;
// production uses the tiered defaults (bcrypt tokens, argon2id credential).
func withSecretHasher(h auth.SecretHasher) serverOption {
	return func(o *serverOptions) {
		o.tokenHasher = h
		o.credentialHasher = h
		o.passwordHasher = h
	}
}

func New(cfg config.Config, db *database.DB, logger *slog.Logger, opts ...serverOption) (*Server, error) {
	options := serverOptions{tokenHasher: auth.BcryptHasher{}, credentialHasher: auth.Argon2Hasher{}, passwordHasher: auth.Argon2Hasher{}}
	for _, opt := range opts {
		opt(&options)
	}
	clients, err := auth.NewClientAuthenticatorWithHasher(db.SQL, options.tokenHasher)
	if err != nil {
		return nil, err
	}
	var sessions *auth.SessionStore
	if cfg.Mode != config.ModeHosted {
		sessions, err = auth.NewSessionStoreTiered(db.SQL, cfg.TillerUser, cfg.TillerUserPassword, cfg.AdminSessionTTL, options.tokenHasher, options.credentialHasher)
		if err != nil {
			return nil, err
		}
	}
	// Configured TTLs are optional: a zero value (e.g. a test config literal)
	// leaves the auth package defaults in place.
	clients.SetCacheTTL(cfg.ClientKeyCacheTTL)
	if sessions != nil {
		sessions.SetCacheTTL(cfg.SessionCacheTTL)
	}
	registry := providers.NewRegistry()
	if cfg.Mode == config.ModeHosted {
		registry = providers.NewHostedRegistry()
	}
	if cfg.ModelsDevEnabled {
		registry.LoadModelsDevCache(filepath.Join(cfg.DataDir, providers.ModelsDevCacheFile()))
	}
	storeOpts := []store.Option{store.WithActivityDB(db.Activity), store.WithLimitEnforcement(cfg.Mode == config.ModeHosted)}
	if options.cipher != nil {
		storeOpts = append(storeOpts, store.WithCipher(options.cipher))
	}
	st := store.New(db.SQL, storeOpts...)
	identityStore, err := identity.New(db.SQL, options.passwordHasher, options.tokenHasher, options.credentialHasher, cfg.UserSessionTTL)
	if err != nil {
		return nil, err
	}
	if cfg.Mode == config.ModeHosted {
		if err := identityStore.BootstrapHostedCustomer(context.Background(), cfg.TillerUser, cfg.TillerUserPassword, db.FreshInstall); err != nil {
			return nil, fmt.Errorf("hosted bootstrap: %w", err)
		}
		if err := identityStore.SyncPlatformCredential(cfg.TillerPlatformAdminUser, cfg.TillerPlatformAdminPassword); err != nil {
			return nil, err
		}
	}
	if cfg.Mail.Configured() {
		if err := st.SeedPlatformMailSettings(context.Background(), platformMailSettings(cfg.Mail)); err != nil && !errors.Is(err, store.ErrSecretsLocked) {
			return nil, err
		}
	}
	mailManager, err := mailer.NewManager(mailer.Config{})
	if err != nil {
		return nil, err
	}
	if saved, loadErr := st.GetPlatformMailSettings(context.Background()); loadErr == nil && saved.Provider != "" {
		if err := mailManager.Update(mailer.Config{Provider: saved.Provider, From: saved.From, ResendAPIKey: saved.ResendAPIKey, BrevoAPIKey: saved.BrevoAPIKey, SMTPHost: saved.SMTPHost, SMTPPort: parseMailPort(saved.SMTPPort), SMTPUsername: saved.SMTPUsername, SMTPPassword: saved.SMTPPassword, SMTPMode: saved.SMTPMode}); err != nil {
			return nil, err
		}
	}
	var outbox *mailoutbox.Outbox
	var mailCipher mailoutbox.Cipher
	if options.cipher != nil {
		mailCipher = options.cipher
	}
	if cfg.Mode == config.ModeHosted {
		deadLetter := func(ctx context.Context, row mailoutbox.DeadLetter) {
			if err := st.RecordPlatformAudit(ctx, store.AuditEvent{Event: "platform.mail_dead_letter", ActorType: "platform", TargetType: "mail", TargetID: row.ID, Metadata: map[string]string{"type": row.Type, "attempts": strconv.Itoa(row.Attempts)}}); err != nil && logger != nil {
				logger.Error("mail dead-letter audit write failed", "error_class", fmt.Sprintf("%T", err))
			}
		}
		outbox = mailoutbox.New(db.SQL, mailManager, cfg.PublicURL, mailCipher, logger, deadLetter)
		identityStore.SetMailQueue(outbox)
	}
	notifyClient := &http.Client{Timeout: notificationTimeout}
	if cfg.Mode == config.ModeHosted {
		notifyClient = hostednet.NewClient(notificationTimeout)
	}
	s := &Server{config: cfg, db: db, store: st, secretCipher: options.cipher, clients: clients, sessions: sessions, identity: identityStore, mailer: mailManager, outbox: outbox, adminAccount: options.adminAccount, secretHasher: options.tokenHasher, providers: providers.NewManager(st, registry), oauthFlows: oauth.NewFlowStore(nil), oauthDevices: map[string]*oauthDeviceState{}, logger: logger, assets: webassets.Handler(), notifyClient: notifyClient, notifyLastSent: map[string]time.Time{}, notifyInFlight: map[string]bool{}, loginLimiter: newLoginLimiter(5, 15*time.Minute, 15*time.Minute), userLoginLimiter: newLoginLimiter(8, 15*time.Minute, 15*time.Minute), signupLimiter: newLoginLimiter(5, time.Hour, time.Hour), recoveryLimiter: newLoginLimiter(5, time.Hour, time.Hour), clientSelectorLimiter: newLoginLimiter(20, time.Minute, time.Minute), clientAddressLimiter: newLoginLimiter(40, time.Minute, time.Minute), oauthStartLimiter: newLoginLimiter(10, time.Minute, time.Minute), oauthCallbackLimiter: newLoginLimiter(10, time.Minute, time.Minute), backgroundCtx: context.Background(), lastOutcome: map[string]lastOutcome{}, liveHub: &liveHub{outcomeCh: make(chan outcomeEvent, liveOutcomeBuffer), activityCh: make(chan activityEvent, liveOutcomeBuffer), timings: liveTimings{debounce: liveDebounceInterval, idle: liveIdleInterval, sessionCheck: liveSessionCheckInterval}}, inflight: &inflightTracker{clientStates: map[string]inflightState{}, targetStates: map[string]inflightState{}}, cooldown: newCooldownStore(), usageAgg: map[string]*usageAggregates{}, usageAggAt: map[string]time.Time{}, usageCacheTTL: usageAggregateTTL}
	s.inflight.emit = s.liveHub.emitActivity
	s.liveHub.snapshot = s.buildUsageSnapshot
	if cfg.Mode == config.ModeHosted {
		if err := s.SeedLegalDocuments(context.Background()); err != nil && logger != nil {
			logger.Warn("legal document seed failed", "error_class", fmt.Sprintf("%T", err))
		}
	}
	return s, nil
}
func (s *Server) StartBackground(ctx context.Context) {
	s.backgroundCtx = ctx
	if err := s.storeHandle().ReconcileActivityCleanup(ctx); err != nil && s.logger != nil {
		s.logger.Warn("activity cleanup reconciliation failed", "error_class", fmt.Sprintf("%T", err))
	}
	s.providers.StartScheduler(ctx)
	if s.config.ModelsDevEnabled {
		s.providers.Registry().StartModelsDevRefresh(ctx, filepath.Join(s.config.DataDir, providers.ModelsDevCacheFile()))
	}
	s.clients.StartSweeper(ctx)
	if s.sessions != nil {
		s.sessions.StartSweeper(ctx)
	}
	if s.identity != nil {
		s.identity.StartSweeper(ctx)
	}
	if s.outbox != nil {
		s.outbox.Start(ctx)
	}
	go s.startLogPruner(ctx)
	s.startLogWriter(ctx)
	go s.startActivityMaintenance(ctx)
	go s.startAuditMaintenance(ctx)
	go s.startMaintenanceScheduler(ctx)
}

// startLogWriter launches the asynchronous Activity writer. It runs only in
// the real deployment (StartBackground); tests that need deterministic reads
// keep the synchronous write path.
func (s *Server) startLogWriter(_ context.Context) {
	w := newLogWriter(s.scopeFor, s.logger)
	s.logWriter = w
	w.start(context.Background())
}

func (s *Server) flushActivity(ctx context.Context) error {
	if s.logWriter == nil {
		return nil
	}
	return s.logWriter.flush(ctx)
}

func (s *Server) finalizeActivityCleanup(ctx context.Context, accountID, clientKeyID string) error {
	cleanupStore := s.storeHandle()
	firstErr := cleanupStore.DeleteActivityRows(ctx, accountID, clientKeyID)
	if s.logWriter != nil {
		if err := s.logWriter.discardAndDrain(ctx, accountID, clientKeyID); err != nil {
			return err
		}
	}
	secondErr := cleanupStore.DeleteActivityRows(ctx, accountID, clientKeyID)
	if secondErr == nil {
		return cleanupStore.RetireActivityCleanup(accountID, clientKeyID)
	}
	if errors.Is(firstErr, store.ErrActivityUnavailable) || errors.Is(secondErr, store.ErrActivityUnavailable) {
		return nil
	}
	return secondErr
}

func (s *Server) StopBackground(ctx context.Context) error {
	if s.logWriter == nil {
		return nil
	}
	return s.logWriter.stop(ctx)
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
	mux.HandleFunc("GET /api/runtime", s.runtime)
	mux.HandleFunc("GET /health/live", s.liveHealth)
	mux.HandleFunc("GET /health/ready", s.ready)
	mux.HandleFunc("GET /health/version", s.versionHealth)
	mux.HandleFunc("GET /security.txt", s.handleSecurityTxt)
	mux.HandleFunc("GET /.well-known/security.txt", s.handleSecurityTxt)
	if s.config.Mode == config.ModeHosted {
		mux.HandleFunc("POST /api/auth/signup", s.signup)
		mux.HandleFunc("POST /api/auth/login", s.userLogin)
		mux.HandleFunc("GET /api/legal/{slug}", s.legalDoc)
		mux.Handle("GET /api/auth/session", s.requireUser(http.HandlerFunc(s.userSessionStatus)))
		mux.Handle("DELETE /api/auth/session", s.requireUser(http.HandlerFunc(s.userLogout)))
		mux.HandleFunc("POST /api/auth/verify-email", s.verifyEmail)
		mux.HandleFunc("POST /api/auth/verification/resend", s.resendVerification)
		mux.HandleFunc("POST /api/auth/password-reset/request", s.requestPasswordReset)
		mux.HandleFunc("POST /api/auth/password-reset/confirm", s.confirmPasswordReset)
		mux.HandleFunc("POST /api/auth/email-change/confirm", s.confirmEmailChange)
		mux.Handle("GET /api/auth/account", s.requireUser(http.HandlerFunc(s.accountProfile)))
		mux.Handle("GET /api/auth/account/plan", s.requireUser(http.HandlerFunc(s.writeAccountPlan)))
		mux.Handle("GET /api/auth/account/export", s.requireUser(http.HandlerFunc(s.writeAccountExport)))
		mux.Handle("GET /api/auth/onboarding", s.requireUser(http.HandlerFunc(s.writeOnboardingState)))
		mux.Handle("POST /api/auth/onboarding/dismiss", s.requireUser(http.HandlerFunc(s.setOnboardingDismissed)))
		mux.Handle("POST /api/auth/account/password", s.requireUser(http.HandlerFunc(s.changeOwnPassword)))
		mux.Handle("POST /api/auth/account/email", s.requireUser(http.HandlerFunc(s.requestOwnEmailChange)))
		mux.Handle("POST /api/auth/account/sessions/revoke-all", s.requireUser(http.HandlerFunc(s.revokeOwnSessions)))
		mux.Handle("DELETE /api/auth/account", s.requireUser(http.HandlerFunc(s.deleteOwnAccount)))
		mux.HandleFunc("POST /api/platform/session", s.platformLogin)
		mux.Handle("GET /api/platform/session", s.requirePlatform(http.HandlerFunc(s.platformSessionStatus)))
		mux.Handle("DELETE /api/platform/session", s.requirePlatform(http.HandlerFunc(s.platformLogout)))
		mux.Handle("GET /api/platform/settings", s.requirePlatform(http.HandlerFunc(s.platformSettings)))
		mux.Handle("PUT /api/platform/settings", s.requirePlatform(http.HandlerFunc(s.updatePlatformSettings)))
		mux.Handle("GET /api/platform/plans", s.requirePlatform(http.HandlerFunc(s.writePlatformPlans)))
		mux.Handle("PUT /api/platform/plans/{name}", s.requirePlatform(http.HandlerFunc(s.writePlatformPlanUpdate)))
		mux.Handle("POST /api/platform/accounts/{id}/plan", s.requirePlatform(http.HandlerFunc(s.writeAccountPlanAssign)))
		mux.Handle("GET /api/platform/legal", s.requirePlatform(http.HandlerFunc(s.writePlatformLegalDocs)))
		mux.Handle("PUT /api/platform/legal/{slug}", s.requirePlatform(http.HandlerFunc(s.writePlatformLegalUpdate)))
		mux.Handle("GET /api/platform/users", s.requirePlatform(http.HandlerFunc(s.platformUsers)))
		mux.Handle("GET /api/platform/audit", s.requirePlatform(http.HandlerFunc(s.platformAudit)))
		mux.Handle("GET /api/platform/mail/queue", s.requirePlatform(http.HandlerFunc(s.platformMailQueue)))
		mux.Handle("POST /api/platform/accounts/{id}/suspend", s.requirePlatform(http.HandlerFunc(s.suspendAccount)))
		mux.Handle("POST /api/platform/accounts/{id}/unsuspend", s.requirePlatform(http.HandlerFunc(s.unsuspendAccount)))
		mux.Handle("POST /api/platform/accounts/{id}/sessions/revoke", s.requirePlatform(http.HandlerFunc(s.revokeAccountSessions)))
		mux.Handle("DELETE /api/platform/accounts/{id}", s.requirePlatform(http.HandlerFunc(s.deleteAccount)))
	} else {
		mux.HandleFunc("POST /api/admin/session", s.login)
		mux.Handle("GET /api/admin/session", s.requireAdmin(http.HandlerFunc(s.sessionStatus)))
		mux.Handle("DELETE /api/admin/session", s.requireAdmin(http.HandlerFunc(s.logout)))
	}
	if s.config.Mode == config.ModeHosted {
		mux.Handle("GET /api/admin/audit", s.requireUser(http.HandlerFunc(s.accountAudit)))
	} else {
		mux.Handle("GET /api/admin/backup/export", s.requireAdmin(http.HandlerFunc(s.exportBackup)))
	}
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
	mux.Handle("POST /api/admin/providers/{id}/models", s.requireAdmin(http.HandlerFunc(s.addManualModel)))
	mux.Handle("GET /api/admin/providers/{id}/models", s.requireAdmin(http.HandlerFunc(s.listProviderModels)))
	mux.Handle("GET /api/admin/providers/{id}/models/lookup", s.requireAdmin(http.HandlerFunc(s.lookupManualModel)))
	mux.Handle("DELETE /api/admin/models/{id}", s.requireAdmin(http.HandlerFunc(s.deleteManualModel)))
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
	mux.Handle("GET /api/admin/cooldown", s.requireAdmin(http.HandlerFunc(s.cooldownStatus)))
	mux.Handle("DELETE /api/admin/cooldown", s.requireAdmin(http.HandlerFunc(s.clearCooldown)))
	mux.Handle("GET /api/admin/health", s.requireAdmin(http.HandlerFunc(s.adminHealth)))
	mux.Handle("GET /api/admin/debug/memory", s.requireAdmin(http.HandlerFunc(s.debugMemory)))
	if s.config.DebugPprof {
		mux.Handle("GET "+debugPprofPrefix, s.requireAdmin(http.HandlerFunc(s.debugPprof)))
	}
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
	if !auth.EqualCredential(input.Username, s.config.TillerUser) || !auth.EqualCredential(input.Password, s.config.TillerUserPassword) {
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
	s.notifyAdminEvent(database.LocalAccountID, eventAdminLogin, fmt.Sprintf("User: %s\nIP: %s", s.config.TillerUser, clientIP(r, s.config.TrustedProxy)))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": s.config.TillerUser, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt.UTC()})
}

// setSessionCookie writes the admin session cookie. Refreshing it on every
// authenticated request keeps the browser cookie's MaxAge in sync with the
// server-side sliding expiry, so active use extends the session across browser
// reopen rather than the cookie expiring 30 days after login.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.secureRequest(r) || s.config.AdminCookieSecure, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(time.Until(expires).Seconds())})
}

func (s *Server) sessionStatus(w http.ResponseWriter, r *http.Request) {
	session := r.Context().Value(adminSessionKey).(auth.Session)
	writeJSON(w, 200, map[string]any{"authenticated": true, "username": s.config.TillerUser, "csrf_token": session.CSRFToken, "expires_at": session.ExpiresAt.UTC()})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureRequest(r) || s.config.AdminCookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) requireAdmin(next http.Handler) http.Handler {
	if s.config.Mode == config.ModeHosted {
		return s.requireUser(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			adminError(w, 401, "unauthorized", "Authentication required.")
			return
		}
		session, ok := s.sessions.Get(cookie.Value)
		if !ok {
			adminError(w, 401, "unauthorized", "Authentication required.")
			return
		}
		// Refresh the cookie so the browser tracks the sliding session expiry.
		s.setSessionCookie(w, r, cookie.Value, session.ExpiresAt)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sessions.CheckCSRF(session, r.Header.Get("X-CSRF-Token")) {
			adminError(w, 403, "csrf_failed", "A valid CSRF token is required.")
			return
		}
		// Phase 1 local mode: the env-admin session owns the single implicit
		// local account. Hosted mode (Phase 3) will resolve the account owned by
		// the authenticated user instead; an injected resolver (tests) stands in.
		accountID := database.LocalAccountID
		if s.adminAccount != nil {
			accountID = s.adminAccount(session)
		}
		ctx := context.WithValue(r.Context(), adminSessionKey, session)
		ctx = context.WithValue(ctx, accountKey, accountID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// scope returns the account-scoped store handle for the request. The account
// is taken from the verified principal attached by the auth middleware.
//
// In local mode a request that reaches a handler without a principal is a
// routing bug, but there is exactly one implicit account, so it falls back to
// the local account id. Hosted mode must never do that: a missing principal
// there would silently resolve to the local account and cross the tenant
// boundary, so it fails closed to an empty (never-matching) account instead.
func (s *Server) scope(r *http.Request) *store.Scope {
	accountID, _ := r.Context().Value(accountKey).(string)
	if accountID == "" {
		if s.config.Mode == config.ModeHosted {
			if s.logger != nil {
				s.logger.Warn("hosted request reached handler without a verified account principal")
			}
			return s.storeHandle().For("")
		}
		accountID = database.LocalAccountID
	}
	return s.storeHandle().For(accountID)
}

// scopeFor returns an account-scoped handle for background work that runs
// outside a request context.
func (s *Server) scopeFor(accountID string) *store.Scope {
	return s.storeHandle().For(accountID)
}

// storeHandle returns the configured tenant store, or a thin wrapper over the
// server's DB when a Server was constructed directly (tests). Production always
// goes through New, which sets store.
func (s *Server) storeHandle() *store.Store {
	if s.store == nil {
		opts := []store.Option{store.WithActivityDB(s.db.Activity)}
		if s.secretCipher != nil {
			opts = append(opts, store.WithCipher(s.secretCipher))
		}
		return store.New(s.db.SQL, opts...)
	}
	return s.store
}

func (s *Server) requireClient(next http.Handler, anthropic bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearerToken(r.Header.Get("Authorization"))
		if raw == "" && anthropic {
			raw = r.Header.Get("x-api-key")
		}
		selector, _, parsed := auth.ParseKey(raw)
		address := s.requestClientIP(r)
		if !parsed || s.clientSelectorLimiter.locked(selector) || s.clientAddressLimiter.locked(address) {
			inferenceError(w, 401, "authentication_error", "invalid_api_key", "Invalid API key.", anthropic)
			return
		}
		identity, ok, available := s.clients.AuthenticateContext(r.Context(), raw)
		if !available {
			w.Header().Set("Retry-After", "1")
			inferenceError(w, http.StatusTooManyRequests, "rate_limited", "authentication_temporarily_unavailable", "Authentication temporarily unavailable.", anthropic)
			return
		}
		if !ok {
			locked := s.clientSelectorLimiter.recordFailure(selector)
			locked = s.clientAddressLimiter.recordFailure(address) || locked
			if locked {
				w.Header().Set("Retry-After", "60")
				inferenceError(w, http.StatusTooManyRequests, "rate_limited", "authentication_rate_limited", "Too many failed authentication attempts. Try again later.", anthropic)
				return
			}
			inferenceError(w, 401, "authentication_error", "invalid_api_key", "Invalid API key.", anthropic)
			return
		}
		s.clientSelectorLimiter.success(selector)
		s.clientAddressLimiter.success(address)
		ctx := context.WithValue(r.Context(), clientKey, identity)
		ctx = context.WithValue(ctx, accountKey, identity.AccountID)
		next.ServeHTTP(w, r.WithContext(ctx))
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

// tenantKey joins an account id with a tenant-variable cache/state key so a
// value for one account can never be served to another.
func tenantKey(accountID, id string) string {
	return accountID + "\x00" + id
}

// rowAccountID resolves the account for a log row, defaulting a row that was
// built without one (older code path or a unit test) to the implicit local
// account.
func rowAccountID(row *logRow) string {
	if row == nil || row.accountID == "" {
		return database.LocalAccountID
	}
	return row.accountID
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// requestClientIP returns the client address suitable for forwarding to an
// anonymous provider. Forwarded headers are accepted only from the configured
// trusted proxy; otherwise the direct peer address is used. When the direct
// peer is trusted, the authoritative X-Real-IP header is preferred, and the
// X-Forwarded-For chain is resolved by the canonical clientIP walker so the
// two helpers can never drift.
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
	return clientIP(r, s.config.TrustedProxy)
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
	if _, err := fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset); err != nil || offset < 0 {
		offset = 0
	}
	// Cap offset so a huge skip cannot force the database to walk the whole
	// table. A client that needs to page deeper than this should refine its
	// search or use the activity export.
	const maxOffset = 10000
	if offset > maxOffset {
		offset = maxOffset
	}
	search = strings.TrimSpace(r.URL.Query().Get("search"))
	return
}

// backupContentType returns a safe Content-Type for a backup download.
// mime.TypeByExtension can return an empty string for some extensions (e.g.
// ".db" is not always registered), so fall back to a generic binary type
// rather than leaving the header blank.
func backupContentType(ext string) string {
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
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
	w.Header().Set("Content-Type", backupContentType(".db"))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Tiller-Secret-Material", "provider-credentials")
	http.ServeFile(w, r, path)
}
