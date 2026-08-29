package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	AdminUsername string
	AdminPassword string
	DataDir       string
	ListenAddr    string
	TrustProxy    bool
}

func Load() (Config, error) {
	c := Config{
		AdminUsername: os.Getenv("TILLER_ADMIN_USERNAME"),
		AdminPassword: os.Getenv("TILLER_ADMIN_PASSWORD"),
		DataDir:       envDefault("TILLER_DATA_DIR", "/data"),
		ListenAddr:    envDefault("TILLER_LISTEN_ADDR", ":8080"),
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
