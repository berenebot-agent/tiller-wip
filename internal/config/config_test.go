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
