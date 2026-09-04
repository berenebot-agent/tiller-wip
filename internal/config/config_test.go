package config

import (
	"net/netip"
	"testing"
)

func TestModelsDevEnabledFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_ADMIN_USERNAME", "admin")
	t.Setenv("TILLER_ADMIN_PASSWORD", "secret")
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

func TestLogLevel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_ADMIN_USERNAME", "admin")
	t.Setenv("TILLER_ADMIN_PASSWORD", "secret")
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
	t.Setenv("TILLER_ADMIN_USERNAME", "admin")
	t.Setenv("TILLER_ADMIN_PASSWORD", "secret")
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
