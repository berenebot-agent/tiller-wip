package config

import (
	"testing"
)

func TestModelsDevEnabledFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_ADMIN_USERNAME", "admin")
	t.Setenv("TILLER_ADMIN_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", dir)

	// Default on when the env var is unset/empty.
	t.Setenv("TILLER_MODELS_DEV_ENABLED", "")
	t.Setenv("TILLER_TRUST_PROXY_HEADERS", "")
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.ModelsDevEnabled {
		t.Error("ModelsDevEnabled should default to true")
	}
	if c.TrustProxy {
		t.Error("TrustProxy should default to false")
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

func TestTrustProxyRequiresTrustedProxy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TILLER_ADMIN_USERNAME", "admin")
	t.Setenv("TILLER_ADMIN_PASSWORD", "secret")
	t.Setenv("TILLER_DATA_DIR", dir)

	// Proxy trust off (default) with no trusted proxy is valid.
	t.Setenv("TILLER_TRUST_PROXY_HEADERS", "false")
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("proxy trust off with no trusted proxy should load: %v", err)
	}
	if c.TrustProxy {
		t.Error("TrustProxy should be false")
	}

	// Proxy trust on with a valid trusted proxy is valid.
	t.Setenv("TILLER_TRUST_PROXY_HEADERS", "true")
	t.Setenv("TILLER_TRUSTED_PROXY", "172.18.0.0/16")
	c, err = Load()
	if err != nil {
		t.Fatalf("proxy trust on with a trusted proxy should load: %v", err)
	}
	if !c.TrustProxy || !c.TrustedProxy.IsValid() {
		t.Error("TrustProxy and TrustedProxy should both be set")
	}

	// Proxy trust on with no trusted proxy is a hard error.
	t.Setenv("TILLER_TRUSTED_PROXY", "")
	if _, err := Load(); err == nil {
		t.Error("TILLER_TRUST_PROXY_HEADERS=true without TILLER_TRUSTED_PROXY should fail to load")
	}

	// Proxy trust on with an invalid trusted proxy is a hard error.
	t.Setenv("TILLER_TRUSTED_PROXY", "not-a-cidr")
	if _, err := Load(); err == nil {
		t.Error("TILLER_TRUST_PROXY_HEADERS=true with an invalid TILLER_TRUSTED_PROXY should fail to load")
	}
}
