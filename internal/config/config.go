package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
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
}

func Load() (Config, error) {
	c := Config{
		AdminUsername:     os.Getenv("TILLER_ADMIN_USERNAME"),
		AdminPassword:     os.Getenv("TILLER_ADMIN_PASSWORD"),
		AdminCookieSecure: false,
		AdminSessionTTL:   30 * 24 * time.Hour,
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
	}
	switch c.LogLevel = strings.ToLower(c.LogLevel); c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("TILLER_LOG_LEVEL must be debug, info, warn, or error, got %q", c.LogLevel)
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
