package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Config struct {
	AdminUsername   string
	AdminPassword   string
	AdminSessionTTL time.Duration
	DataDir         string
	ListenAddr      string
	TrustProxy      bool
}

func Load() (Config, error) {
	c := Config{
		AdminUsername:   os.Getenv("TILLER_ADMIN_USERNAME"),
		AdminPassword:   os.Getenv("TILLER_ADMIN_PASSWORD"),
		AdminSessionTTL: 30 * 24 * time.Hour,
		DataDir:         envDefault("TILLER_DATA_DIR", "/data"),
		ListenAddr:      envDefault("TILLER_LISTEN_ADDR", ":8080"),
	}
	if raw := os.Getenv("TILLER_ADMIN_SESSION_TTL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_ADMIN_SESSION_TTL: %w", err)
		}
		c.AdminSessionTTL = v
	}
	if raw := os.Getenv("TILLER_TRUST_PROXY_HEADERS"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_TRUST_PROXY_HEADERS: %w", err)
		}
		c.TrustProxy = v
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
	return c, nil
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
