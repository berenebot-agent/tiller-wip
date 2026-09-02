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
	AdminUsername    string
	AdminPassword    string
	AdminSessionTTL  time.Duration
	DataDir          string
	ListenAddr       string
	TrustedProxy     netip.Prefix
	ModelsDevEnabled bool
}

func Load() (Config, error) {
	c := Config{
		AdminUsername:    os.Getenv("TILLER_ADMIN_USERNAME"),
		AdminPassword:    os.Getenv("TILLER_ADMIN_PASSWORD"),
		AdminSessionTTL:  30 * 24 * time.Hour,
		DataDir:          envDefault("TILLER_DATA_DIR", "/data"),
		ListenAddr:       envDefault("TILLER_LISTEN_ADDR", ":8080"),
		ModelsDevEnabled: true,
	}
	if raw := os.Getenv("TILLER_ADMIN_SESSION_TTL"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("TILLER_ADMIN_SESSION_TTL: %w", err)
		}
		c.AdminSessionTTL = v
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
