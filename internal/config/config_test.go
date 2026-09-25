package config

import (
	"net/netip"
	"testing"
	"time"
)

func TestModelsDevEnabledFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_PLATFORM_ADMIN_USERNAME", "platform-admin")
	t.Setenv("TILLER_PLATFORM_ADMIN_PASSWORD", "platform-secret")
	t.Setenv("TILLER_DATA_DIR", dir)

	// Default on when the env var is unset/empty.
	t.Setenv("TILLER_MODELS_DEV_ENABLED", "")
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ModelsDevEnabled {
		t.Error("ModelsDevEnabled should default to true")
	}

	// Explicitly off.
	t.Setenv("TILLER_MODELS_DEV_ENABLED", "false")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelsDevEnabled {
		t.Error("ModelsDevEnabled should be false when TILLER_MODELS_DEV_ENABLED=false")
	}

	// Explicitly on.
	t.Setenv("TILLER_MODELS_DEV_ENABLED", "true")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ModelsDevEnabled {
		t.Error("ModelsDevEnabled should be true when TILLER_MODELS_DEV_ENABLED=true")
	}

	// An invalid value is a hard configuration error, not a silent default.
	t.Setenv("TILLER_MODELS_DEV_ENABLED", "banana")
	if _, err := Load(); err == nil {
		t.Error("TILLER_MODELS_DEV_ENABLED=banana should fail to load")
	}
}

func TestDebugPprofFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_PLATFORM_ADMIN_USERNAME", "platform-admin")
	t.Setenv("TILLER_PLATFORM_ADMIN_PASSWORD", "platform-secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")

	// Off by default: telemetry is env-required.
	t.Setenv("TILLER_DEBUG_PPROF", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DebugPprof {
		t.Error("DebugPprof should default to false")
	}

	// Explicitly on.
	t.Setenv("TILLER_DEBUG_PPROF", "true")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.DebugPprof {
		t.Error("DebugPprof should be true when TILLER_DEBUG_PPROF=true")
	}

	// Explicitly off.
	t.Setenv("TILLER_DEBUG_PPROF", "false")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.DebugPprof {
		t.Error("DebugPprof should be false when TILLER_DEBUG_PPROF=false")
	}

	// An invalid value is a hard configuration error, not a silent default.
	t.Setenv("TILLER_DEBUG_PPROF", "banana")
	if _, err := Load(); err == nil {
		t.Error("TILLER_DEBUG_PPROF=banana should fail to load")
	}
}

func TestHostedDebugPprofRequiresPlatformCredentials(t *testing.T) {
	t.Setenv("TILLER_MODE", "hosted")
	t.Setenv("TILLER_PUBLIC_URL", "https://tiller.example.com")
	t.Setenv("TILLER_TRUSTED_PROXY", "127.0.0.1/32")
	t.Setenv("TILLER_DEBUG_PPROF", "true")
	t.Setenv("TILLER_PLATFORM_ADMIN_USERNAME", "")
	t.Setenv("TILLER_PLATFORM_ADMIN_PASSWORD", "")

	if _, err := Load(); err == nil {
		t.Fatal("hosted TILLER_DEBUG_PPROF=true without platform credentials should fail")
	}

	t.Setenv("TILLER_PLATFORM_ADMIN_USERNAME", "platform-admin")
	t.Setenv("TILLER_PLATFORM_ADMIN_PASSWORD", "platform-secret")
	if _, err := Load(); err != nil {
		t.Fatalf("hosted TILLER_DEBUG_PPROF=true with platform credentials: %v", err)
	}
}

func TestCustomSiteEnabledFlag(t *testing.T) {
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", t.TempDir())
	t.Setenv("TILLER_CUSTOM_SITE_ENABLED", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.CustomSiteEnabled {
		t.Fatal("CustomSiteEnabled should default to false")
	}

	t.Setenv("TILLER_CUSTOM_SITE_ENABLED", "true")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.CustomSiteEnabled {
		t.Fatal("CustomSiteEnabled should be true when enabled")
	}

	t.Setenv("TILLER_CUSTOM_SITE_ENABLED", "banana")
	if _, err := Load(); err == nil {
		t.Fatal("invalid TILLER_CUSTOM_SITE_ENABLED should fail to load")
	}
}

func TestCacheTTLFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")

	t.Setenv("TILLER_CLIENT_KEY_CACHE_TTL", "")
	t.Setenv("TILLER_SESSION_CACHE_TTL", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientKeyCacheTTL != 15*time.Minute {
		t.Errorf("ClientKeyCacheTTL default = %v, want 15m", c.ClientKeyCacheTTL)
	}
	if c.SessionCacheTTL != 5*time.Minute {
		t.Errorf("SessionCacheTTL default = %v, want 5m", c.SessionCacheTTL)
	}

	t.Setenv("TILLER_CLIENT_KEY_CACHE_TTL", "30m")
	t.Setenv("TILLER_SESSION_CACHE_TTL", "90s")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientKeyCacheTTL != 30*time.Minute {
		t.Errorf("ClientKeyCacheTTL override = %v, want 30m", c.ClientKeyCacheTTL)
	}
	if c.SessionCacheTTL != 90*time.Second {
		t.Errorf("SessionCacheTTL override = %v, want 90s", c.SessionCacheTTL)
	}

	// A too-large value is clamped rather than rejected.
	t.Setenv("TILLER_CLIENT_KEY_CACHE_TTL", "720h")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.ClientKeyCacheTTL != 24*time.Hour {
		t.Errorf("ClientKeyCacheTTL clamp = %v, want 24h", c.ClientKeyCacheTTL)
	}

	for _, bad := range []string{"0s", "-5m", "banana"} {
		t.Setenv("TILLER_CLIENT_KEY_CACHE_TTL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("TILLER_CLIENT_KEY_CACHE_TTL=%q should fail to load", bad)
		}
	}
}

func TestModeAndPublicURL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin@example.com")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_PLATFORM_ADMIN_USERNAME", "platform-admin")
	t.Setenv("TILLER_PLATFORM_ADMIN_PASSWORD", "platform-secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	t.Setenv("TILLER_PUBLIC_URL", "")
	t.Setenv("TILLER_MODE", "")

	// Unset mode defaults to local, and local does not require a public URL.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeLocal {
		t.Errorf("Mode = %q, want local by default", c.Mode)
	}
	if c.PublicURL != "" {
		t.Errorf("PublicURL = %q, want empty in local mode", c.PublicURL)
	}

	// Explicit local is accepted.
	t.Setenv("TILLER_MODE", "local")
	if c, err = Load(); err != nil || c.Mode != ModeLocal {
		t.Fatalf("TILLER_MODE=local: mode=%q err=%v", c.Mode, err)
	}

	// Hosted without a public URL is a hard error, even if proxy trust is set.
	t.Setenv("TILLER_MODE", "hosted")
	t.Setenv("TILLER_TRUSTED_PROXY", "172.18.0.1")
	if _, err := Load(); err == nil {
		t.Error("hosted mode without TILLER_PUBLIC_URL should fail")
	}

	// Hosted with a valid origin but no trusted proxy fails closed.
	t.Setenv("TILLER_PUBLIC_URL", "https://app.example.com")
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	if _, err := Load(); err == nil {
		t.Error("hosted mode without TILLER_TRUSTED_PROXY should fail")
	}

	// Hosted with a valid origin and proxy succeeds and preserves the origin.
	t.Setenv("TILLER_TRUSTED_PROXY", "172.18.0.1")
	c, err = Load()
	if err != nil {
		t.Fatalf("hosted with public URL should load: %v", err)
	}
	if c.Mode != ModeHosted || c.PublicURL != "https://app.example.com" {
		t.Errorf("mode=%q publicURL=%q", c.Mode, c.PublicURL)
	}

	// An invalid mode is a hard error.
	t.Setenv("TILLER_MODE", "banana")
	if _, err := Load(); err == nil {
		t.Error("TILLER_MODE=banana should fail to load")
	}
}

func TestPublicURLValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin@example.com")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_PLATFORM_ADMIN_USERNAME", "platform-admin")
	t.Setenv("TILLER_PLATFORM_ADMIN_PASSWORD", "platform-secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "172.18.0.1")
	t.Setenv("TILLER_MODE", "hosted")

	bad := []string{
		"http://app.example.com",
		"https://app.example.com/path",
		"https://app.example.com/?q=1",
		"https://app.example.com/#frag",
		"https://user:pass@app.example.com",
		"https://*.example.com",
		"not a url",
		"https://",
	}
	for _, raw := range bad {
		t.Setenv("TILLER_PUBLIC_URL", raw)
		if _, err := Load(); err == nil {
			t.Errorf("TILLER_PUBLIC_URL=%q should fail to load", raw)
		}
	}

	// A trailing slash is normalized away.
	t.Setenv("TILLER_PUBLIC_URL", "https://app.example.com/")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicURL != "https://app.example.com" {
		t.Errorf("PublicURL = %q, want normalized origin", c.PublicURL)
	}
}

func TestUserSessionTTL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	t.Setenv("TILLER_MODE", "")
	t.Setenv("TILLER_PUBLIC_URL", "")

	t.Setenv("TILLER_USER_SESSION_TTL", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.UserSessionTTL != 30*24*time.Hour {
		t.Errorf("UserSessionTTL default = %v, want 720h", c.UserSessionTTL)
	}

	t.Setenv("TILLER_USER_SESSION_TTL", "48h")
	if c, err = Load(); err != nil || c.UserSessionTTL != 48*time.Hour {
		t.Fatalf("override: ttl=%v err=%v", c.UserSessionTTL, err)
	}

	for _, bad := range []string{"0s", "-1h", "banana"} {
		t.Setenv("TILLER_USER_SESSION_TTL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("TILLER_USER_SESSION_TTL=%q should fail to load", bad)
		}
	}
}

func TestMailBootstrap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	t.Setenv("TILLER_MODE", "")
	t.Setenv("TILLER_PUBLIC_URL", "")
	clearMailEnv := func() {
		for _, k := range []string{"TILLER_MAIL_PROVIDER", "TILLER_MAIL_FROM", "TILLER_MAIL_SMTP_HOST", "TILLER_MAIL_SMTP_PORT", "TILLER_MAIL_SMTP_USERNAME", "TILLER_MAIL_SMTP_PASSWORD", "TILLER_MAIL_SMTP_MODE"} {
			t.Setenv(k, "")
		}
	}
	clearMailEnv()

	// No mail settings: zero value, no error.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Mail.Configured() {
		t.Error("mail should not be configured by default")
	}

	// SMTP defaults to port 587/starttls.
	clearMailEnv()
	t.Setenv("TILLER_MAIL_PROVIDER", "smtp")
	t.Setenv("TILLER_MAIL_FROM", "no-reply@example.com")
	t.Setenv("TILLER_MAIL_SMTP_HOST", "smtp.example.com")
	c, err = Load()
	if err != nil {
		t.Fatalf("smtp mail should load: %v", err)
	}
	if c.Mail.SMTPPort != 587 || c.Mail.SMTPMode != "starttls" {
		t.Errorf("smtp defaults = %+v", c.Mail)
	}

	// Implicit TLS defaults to port 465.
	clearMailEnv()
	t.Setenv("TILLER_MAIL_PROVIDER", "smtp")
	t.Setenv("TILLER_MAIL_FROM", "no-reply@example.com")
	t.Setenv("TILLER_MAIL_SMTP_HOST", "email-smtp.us-east-1.amazonaws.com")
	t.Setenv("TILLER_MAIL_SMTP_MODE", "implicit")
	c, err = Load()
	if err != nil {
		t.Fatalf("smtp implicit mail should load: %v", err)
	}
	if c.Mail.SMTPPort != 465 || c.Mail.SMTPMode != "implicit" {
		t.Errorf("smtp implicit = %+v", c.Mail)
	}

	// A half-configured / invalid mail config fails loud.
	cases := []map[string]string{
		{"TILLER_MAIL_PROVIDER": "pigeon", "TILLER_MAIL_FROM": "x@y.z"},
		{"TILLER_MAIL_PROVIDER": "resend", "TILLER_MAIL_FROM": "x@y.z"},
		{"TILLER_MAIL_PROVIDER": "smtp", "TILLER_MAIL_FROM": "x@y.z"},
		{"TILLER_MAIL_PROVIDER": "smtp", "TILLER_MAIL_SMTP_HOST": "h"},
		{"TILLER_MAIL_PROVIDER": "smtp", "TILLER_MAIL_FROM": "x@y.z", "TILLER_MAIL_SMTP_HOST": "h", "TILLER_MAIL_SMTP_MODE": "plain"},
		{"TILLER_MAIL_PROVIDER": "smtp", "TILLER_MAIL_FROM": "x@y.z", "TILLER_MAIL_SMTP_HOST": "h", "TILLER_MAIL_SMTP_PORT": "70000"},
		{"TILLER_MAIL_PROVIDER": "smtp", "TILLER_MAIL_FROM": "x@y.z", "TILLER_MAIL_SMTP_HOST": "h", "TILLER_MAIL_SMTP_USERNAME": "u"},
	}
	for i, env := range cases {
		clearMailEnv()
		for k, v := range env {
			t.Setenv(k, v)
		}
		if _, err := Load(); err == nil {
			t.Errorf("mail case %d (%v) should fail to load", i, env)
		}
	}
}

func TestLogLevel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", dir)

	// Default when unset.
	t.Setenv("TILLER_LOG_LEVEL", "")
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want default info", c.LogLevel)
	}

	// Explicit value is preserved.
	t.Setenv("TILLER_LOG_LEVEL", "warn")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn", c.LogLevel)
	}

	// Case-insensitive.
	t.Setenv("TILLER_LOG_LEVEL", "WARN")
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn (case-insensitive)", c.LogLevel)
	}

	// Invalid value is a hard configuration error.
	t.Setenv("TILLER_LOG_LEVEL", "banana")
	if _, err := Load(); err == nil {
		t.Error("TILLER_LOG_LEVEL=banana should fail to load")
	}
}

func TestTrustedProxy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", dir)

	// Unset (default) means proxy-header trust is disabled.
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("no trusted proxy should load: %v", err)
	}
	if c.TrustedProxy.IsValid() {
		t.Error("TrustedProxy should be invalid when unset")
	}

	// A valid CIDR enables proxy-header trust.
	t.Setenv("TILLER_TRUSTED_PROXY", "172.18.0.0/16")
	c, err = Load()
	if err != nil {
		t.Fatalf("valid trusted proxy should load: %v", err)
	}
	if !c.TrustedProxy.IsValid() {
		t.Error("TrustedProxy should be set")
	}
	if c.TrustedProxy != netip.MustParsePrefix("172.18.0.0/16") {
		t.Errorf("TrustedProxy = %s, want 172.18.0.0/16", c.TrustedProxy)
	}

	// A bare proxy address is treated as a single-address CIDR.
	t.Setenv("TILLER_TRUSTED_PROXY", "10.1.1.18")
	c, err = Load()
	if err != nil {
		t.Fatalf("bare trusted proxy address should load: %v", err)
	}
	if c.TrustedProxy != netip.MustParsePrefix("10.1.1.18/32") {
		t.Errorf("TrustedProxy = %s, want 10.1.1.18/32", c.TrustedProxy)
	}

	// A bare IP (no /prefix) is treated as /32.
	t.Setenv("TILLER_TRUSTED_PROXY", "10.1.1.18")
	c, err = Load()
	if err != nil {
		t.Fatalf("bare IP for TILLER_TRUSTED_PROXY should load as /32: %v", err)
	}
	if !c.TrustedProxy.IsValid() || c.TrustedProxy.String() != "10.1.1.18/32" {
		t.Errorf("TrustedProxy should be 10.1.1.18/32, got %v", c.TrustedProxy)
	}

	// A bare IPv6 address is treated as /128, not widened to /32.
	t.Setenv("TILLER_TRUSTED_PROXY", "2001:db8::1234")
	c, err = Load()
	if err != nil {
		t.Fatalf("bare IPv6 for TILLER_TRUSTED_PROXY should load as /128: %v", err)
	}
	if !c.TrustedProxy.IsValid() || c.TrustedProxy.String() != "2001:db8::1234/128" {
		t.Errorf("TrustedProxy should be 2001:db8::1234/128, got %v", c.TrustedProxy)
	}

	// An explicit IPv6 CIDR is preserved verbatim.
	t.Setenv("TILLER_TRUSTED_PROXY", "2001:db8::/32")
	c, err = Load()
	if err != nil {
		t.Fatalf("IPv6 CIDR for TILLER_TRUSTED_PROXY should load: %v", err)
	}
	if !c.TrustedProxy.IsValid() || c.TrustedProxy.String() != "2001:db8::/32" {
		t.Errorf("TrustedProxy should be 2001:db8::/32, got %v", c.TrustedProxy)
	}

	// An invalid trusted proxy is a hard error.
	t.Setenv("TILLER_TRUSTED_PROXY", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Error("TILLER_TRUSTED_PROXY=not-a-cidr should fail to load")
	}
}

// TestLegacyCredentialAliasHonoured proves the pre-rename TILLER_ADMIN_* names
// still configure the local operator credential and produce a deprecation
// notice, so an existing deployment survives the rename untouched.
func TestLegacyCredentialAliasHonoured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "")
	t.Setenv("TILLER_PASSWORD", "")
	t.Setenv("TILLER_ADMIN_USERNAME", "legacy-admin")
	t.Setenv("TILLER_ADMIN_PASSWORD", "legacy-secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	t.Setenv("TILLER_MODE", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("legacy credentials should load: %v", err)
	}
	if c.TillerUser != "legacy-admin" || c.TillerUserPassword != "legacy-secret" {
		t.Errorf("legacy alias not applied: user=%q pass=%q", c.TillerUser, c.TillerUserPassword)
	}
	if len(c.Deprecations) != 2 {
		t.Fatalf("Deprecations = %v, want one per legacy var", c.Deprecations)
	}
}

// TestNewCredentialWinsOverLegacy proves that when both names are set the new
// name is authoritative and the ignored legacy var is reported, so a
// half-migrated .env can never silently change which credential is trusted.
func TestNewCredentialWinsOverLegacy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "new-admin")
	t.Setenv("TILLER_PASSWORD", "new-secret")
	t.Setenv("TILLER_ADMIN_USERNAME", "old-admin")
	t.Setenv("TILLER_ADMIN_PASSWORD", "old-secret")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	t.Setenv("TILLER_MODE", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("both names should load: %v", err)
	}
	if c.TillerUser != "new-admin" || c.TillerUserPassword != "new-secret" {
		t.Errorf("new name should win: user=%q pass=%q", c.TillerUser, c.TillerUserPassword)
	}
	if len(c.Deprecations) != 2 {
		t.Fatalf("Deprecations = %v, want one per ignored legacy var", c.Deprecations)
	}
}

// TestNoDeprecationWhenOnlyNewNamesSet keeps the clean path quiet: a fully
// migrated deployment must not emit any startup notice.
func TestNoDeprecationWhenOnlyNewNamesSet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_USERNAME", "admin")
	t.Setenv("TILLER_PASSWORD", "secret")
	t.Setenv("TILLER_ADMIN_USERNAME", "")
	t.Setenv("TILLER_ADMIN_PASSWORD", "")
	t.Setenv("TILLER_DATA_DIR", dir)
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	t.Setenv("TILLER_MODE", "")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Deprecations) != 0 {
		t.Errorf("Deprecations = %v, want none for new-only config", c.Deprecations)
	}
}
