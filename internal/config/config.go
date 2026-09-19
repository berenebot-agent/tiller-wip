package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Mode is the deployment mode. Local is the default and preserves the
// self-hosted appliance behaviour; hosted enables the multi-tenant user stack.
type Mode string

const (
	ModeLocal  Mode = "local"
	ModeHosted Mode = "hosted"
)

// MailBootstrap is the optional mail configuration supplied via environment
// variables. It is used once, as a seed, when the platform has no persisted
// mail configuration; the database is authoritative after that. Secrets here
// are never returned by an API.
type MailBootstrap struct {
	Provider     string // "resend", "ses", or "smtp"
	From         string
	ResendAPIKey string
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPMode     string // "starttls" or "implicit"
}

// Configured reports whether any mail bootstrap setting was supplied.
func (m MailBootstrap) Configured() bool {
	return m.Provider != "" || m.From != "" || m.ResendAPIKey != "" || m.SMTPHost != ""
}

type Config struct {
	// Mode selects local (default) or hosted. An unset/empty TILLER_MODE is
	// local, so existing deployments keep their behaviour unchanged.
	Mode Mode
	// PublicURL is the public HTTPS origin used to build verification and
	// password-reset links. It is required in hosted mode and must be an
	// origin only (no path/query/fragment/credentials/wildcard). It is never
	// derived from request headers.
	PublicURL string
	// UserSessionTTL is the sliding lifetime of a hosted customer session.
	UserSessionTTL time.Duration
	// Mail is the optional bootstrap seed for mail delivery.
	Mail              MailBootstrap
	AdminUsername     string
	AdminPassword     string
	AdminCookieSecure bool
	AdminSessionTTL   time.Duration
	DataDir           string
	ListenAddr        string
	TrustedProxy      netip.Prefix
	ModelsDevEnabled  bool
	LogLevel          string
	// DebugPprof enables the admin-gated memory/pprof debug endpoints. It is
	// off by default and only turns on when TILLER_DEBUG_PPROF is explicitly
	// true, so a normal deployment never exposes profiling surfaces.
	DebugPprof bool
	// ClientKeyCacheTTL is how long a verified client key is trusted by the
	// in-memory auth cache. Verification is immediate on a cache miss and
	// entries renew on use, so this bounds verification cost at scale. Any
	// client-key mutation invalidates the cache regardless of TTL.
	ClientKeyCacheTTL time.Duration
	// SessionCacheTTL is the equivalent in-memory cache window for admin
	// sessions. Session revocation is immediate regardless of TTL.
	SessionCacheTTL time.Duration
	// BackupDir is where scheduled central-database snapshots are written.
	// Defaults to <DataDir>/backups.
	BackupDir string
	// BackupInterval is how often a snapshot is taken. Zero disables scheduled
	// backups.
	BackupInterval time.Duration
	// BackupRetention is how long snapshots are kept before pruning. Off-host
	// copies are the operator's responsibility (see docs/backup_restore_runbook.md).
	BackupRetention time.Duration
	// MasterKey is the raw TILLER_MASTER_KEY value (base64 of 32 random
	// bytes). MasterKeyFile is the raw TILLER_MASTER_KEY_FILE path. The file
	// takes precedence. When both are empty, the key is loaded from
	// <DataDir>/master.key or generated there (see internal/crypto).
	MasterKey     string
	MasterKeyFile string
}

func Load() (Config, error) {
	c := Config{
		AdminUsername:     os.Getenv("TILLER_ADMIN_USERNAME"),
		AdminPassword:     os.Getenv("TILLER_ADMIN_PASSWORD"),
		AdminCookieSecure: false,
		AdminSessionTTL:   30 * 24 * time.Hour,
		UserSessionTTL:    30 * 24 * time.Hour,
		// Verified keys/sessions are cached in memory and renewed on use, so a
		// longer window cuts hash-verification CPU with no revocation penalty:
		// explicit invalidation is independent of the TTL.
		ClientKeyCacheTTL: 15 * time.Minute,
		SessionCacheTTL:   5 * time.Minute,
		DataDir:           envDefault("TILLER_DATA_DIR", "/data"),
		ListenAddr:        envDefault("TILLER_LISTEN_ADDR", ":8080"),
		ModelsDevEnabled:  true,
		LogLevel:          envDefault("TILLER_LOG_LEVEL", "info"),
		BackupInterval:    6 * time.Hour,
		BackupRetention:   7 * 24 * time.Hour,
		MasterKey:         os.Getenv("TILLER_MASTER_KEY"),
		MasterKeyFile:     os.Getenv("TILLER_MASTER_KEY_FILE"),
	}
	switch c.LogLevel = strings.ToLower(c.LogLevel); c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("TILLER_LOG_LEVEL must be debug, info, warn, or error, got %q", c.LogLevel)
	}
	// Mode is opt-in: unset/empty means local, so existing self-hosted
	// deployments keep their behaviour with no new configuration.
	switch raw := strings.ToLower(strings.TrimSpace(os.Getenv("TILLER_MODE"))); raw {
	case "", string(ModeLocal):
		c.Mode = ModeLocal
	case string(ModeHosted):
		c.Mode = ModeHosted
	default:
		return Config{}, fmt.Errorf("TILLER_MODE must be local or hosted, got %q", raw)
	}
	if raw := strings.TrimSpace(os.Getenv("TILLER_PUBLIC_URL")); raw != "" {
		origin, err := validatePublicURL(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_PUBLIC_URL: %w", err)
		}
		c.PublicURL = origin
	}
	if raw := os.Getenv("TILLER_USER_SESSION_TTL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_USER_SESSION_TTL: %w", err)
		}
		if v <= 0 {
			return Config{}, fmt.Errorf("TILLER_USER_SESSION_TTL must be positive, got %q", raw)
		}
		c.UserSessionTTL = v
	}
	mail, err := loadMailBootstrap()
	if err != nil {
		return Config{}, err
	}
	c.Mail = mail
	if c.Mode == ModeHosted && c.PublicURL == "" {
		return Config{}, errors.New("TILLER_PUBLIC_URL is required in hosted mode")
	}
	if raw := os.Getenv("TILLER_ADMIN_COOKIE_SECURE"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_ADMIN_COOKIE_SECURE: %w", err)
		}
		c.AdminCookieSecure = v
	}
	if raw := os.Getenv("TILLER_ADMIN_SESSION_TTL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_ADMIN_SESSION_TTL: %w", err)
		}
		c.AdminSessionTTL = v
	}
	if raw := os.Getenv("TILLER_CLIENT_KEY_CACHE_TTL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_CLIENT_KEY_CACHE_TTL: %w", err)
		}
		if v <= 0 {
			return Config{}, fmt.Errorf("TILLER_CLIENT_KEY_CACHE_TTL must be positive, got %q", raw)
		}
		c.ClientKeyCacheTTL = clampCacheTTL(v)
	}
	if raw := os.Getenv("TILLER_SESSION_CACHE_TTL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_SESSION_CACHE_TTL: %w", err)
		}
		if v <= 0 {
			return Config{}, fmt.Errorf("TILLER_SESSION_CACHE_TTL must be positive, got %q", raw)
		}
		c.SessionCacheTTL = clampCacheTTL(v)
	}
	// Setting TILLER_TRUSTED_PROXY to a CIDR is the switch that enables
	// proxy-header trust: forwarded headers are only honoured when the direct
	// peer is inside that CIDR, so a spoofable header can never be trusted
	// from an untrusted peer. Leaving it unset disables proxy-header trust.
	if raw := os.Getenv("TILLER_TRUSTED_PROXY"); raw != "" {
		var v netip.Prefix
		if strings.Contains(raw, "/") {
			parsed, err := netip.ParsePrefix(raw)
			if err != nil {
				return Config{}, fmt.Errorf("TILLER_TRUSTED_PROXY: %w", err)
			}
			v = parsed
		} else {
			addr, err := netip.ParseAddr(raw)
			if err != nil {
				return Config{}, fmt.Errorf("TILLER_TRUSTED_PROXY: %w", err)
			}
			v = netip.PrefixFrom(addr, addr.BitLen())
		}
		c.TrustedProxy = v
	}
	if raw := os.Getenv("TILLER_MODELS_DEV_ENABLED"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_MODELS_DEV_ENABLED: %w", err)
		}
		c.ModelsDevEnabled = v
	}
	if raw := os.Getenv("TILLER_DEBUG_PPROF"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_DEBUG_PPROF: %w", err)
		}
		c.DebugPprof = v
	}
	if raw := os.Getenv("TILLER_BACKUP_INTERVAL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_BACKUP_INTERVAL: %w", err)
		}
		if v < 0 {
			return Config{}, fmt.Errorf("TILLER_BACKUP_INTERVAL must not be negative, got %q", raw)
		}
		c.BackupInterval = v
	}
	if raw := os.Getenv("TILLER_BACKUP_RETENTION"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_BACKUP_RETENTION: %w", err)
		}
		if v < 0 {
			return Config{}, fmt.Errorf("TILLER_BACKUP_RETENTION must not be negative, got %q", raw)
		}
		c.BackupRetention = v
	}
	if raw := os.Getenv("TILLER_BACKUP_DIR"); raw != "" {
		c.BackupDir = raw
	}
	if c.AdminUsername == "" || c.AdminPassword == "" {
		return Config{}, errors.New("TILLER_ADMIN_USERNAME and TILLER_ADMIN_PASSWORD are required")
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return Config{}, fmt.Errorf("create data directory: %w", err)
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data directory: %w", err)
	}
	c.DataDir = abs
	if c.BackupDir == "" {
		c.BackupDir = filepath.Join(c.DataDir, "backups")
	} else {
		absBackup, err := filepath.Abs(c.BackupDir)
		if err != nil {
			return Config{}, fmt.Errorf("resolve backup directory: %w", err)
		}
		c.BackupDir = absBackup
	}
	return c, nil
}

// validatePublicURL normalizes and validates TILLER_PUBLIC_URL. Only an HTTPS
// origin is accepted: no path (beyond "/"), query, fragment, credentials, or
// wildcard host. Auth links are built from this value, so it must never be
// attacker-controllable.
func validatePublicURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", errors.New("must use https")
	}
	if u.Host == "" {
		return "", errors.New("missing host")
	}
	if u.User != nil {
		return "", errors.New("must not contain credentials")
	}
	if u.Path != "" && u.Path != "/" {
		return "", errors.New("must be an origin only (no path)")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must be an origin only (no query or fragment)")
	}
	if strings.Contains(u.Host, "*") {
		return "", errors.New("must not contain a wildcard host")
	}
	return "https://" + u.Host, nil
}

// loadMailBootstrap reads the optional mail seed from the environment. When no
// mail setting is present it returns the zero value. When any is present the
// configuration must be complete and internally consistent so a half-configured
// deployment fails at startup rather than at first send.
func loadMailBootstrap() (MailBootstrap, error) {
	m := MailBootstrap{
		Provider:     strings.ToLower(strings.TrimSpace(os.Getenv("TILLER_MAIL_PROVIDER"))),
		From:         strings.TrimSpace(os.Getenv("TILLER_MAIL_FROM")),
		ResendAPIKey: strings.TrimSpace(os.Getenv("TILLER_MAIL_RESEND_API_KEY")),
		SMTPHost:     strings.TrimSpace(os.Getenv("TILLER_MAIL_SMTP_HOST")),
		SMTPUsername: strings.TrimSpace(os.Getenv("TILLER_MAIL_SMTP_USERNAME")),
		SMTPPassword: os.Getenv("TILLER_MAIL_SMTP_PASSWORD"),
		SMTPMode:     strings.ToLower(strings.TrimSpace(os.Getenv("TILLER_MAIL_SMTP_MODE"))),
	}
	if !m.Configured() {
		return MailBootstrap{}, nil
	}
	switch m.Provider {
	case "resend", "ses", "smtp":
	default:
		return MailBootstrap{}, fmt.Errorf("TILLER_MAIL_PROVIDER must be resend, ses, or smtp, got %q", m.Provider)
	}
	if m.From == "" {
		return MailBootstrap{}, errors.New("TILLER_MAIL_FROM is required when mail is configured")
	}
	if m.Provider == "resend" {
		if m.ResendAPIKey == "" {
			return MailBootstrap{}, errors.New("TILLER_MAIL_RESEND_API_KEY is required for the resend provider")
		}
		return m, nil
	}
	// "ses" delivers through the generic SMTP adapter.
	if m.SMTPHost == "" {
		return MailBootstrap{}, errors.New("TILLER_MAIL_SMTP_HOST is required for the ses/smtp provider")
	}
	if m.SMTPMode == "" {
		m.SMTPMode = "starttls"
	}
	switch m.SMTPMode {
	case "starttls", "implicit":
	default:
		return MailBootstrap{}, fmt.Errorf("TILLER_MAIL_SMTP_MODE must be starttls or implicit, got %q", m.SMTPMode)
	}
	if raw := strings.TrimSpace(os.Getenv("TILLER_MAIL_SMTP_PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return MailBootstrap{}, fmt.Errorf("TILLER_MAIL_SMTP_PORT must be a valid port, got %q", raw)
		}
		m.SMTPPort = port
	}
	if m.SMTPPort == 0 {
		if m.SMTPMode == "implicit" {
			m.SMTPPort = 465
		} else {
			m.SMTPPort = 587
		}
	}
	if m.SMTPUsername != "" && m.SMTPPassword == "" {
		return MailBootstrap{}, errors.New("TILLER_MAIL_SMTP_PASSWORD is required when TILLER_MAIL_SMTP_USERNAME is set")
	}
	return m, nil
}

// clampCacheTTL bounds a configured auth-cache TTL so a typo cannot pin an
// entry (and the identity it trusts) for an unbounded time. The 24h ceiling is
// well above any sensible value; 1s is the floor for meaningful caching.
func clampCacheTTL(v time.Duration) time.Duration {
	const (
		minTTL = time.Second
		maxTTL = 24 * time.Hour
	)
	if v < minTTL {
		return minTTL
	}
	if v > maxTTL {
		return maxTTL
	}
	return v
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
