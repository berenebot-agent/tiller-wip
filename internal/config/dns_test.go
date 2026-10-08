package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestDNSFallbackConfig(t *testing.T) {
	t.Setenv("TILLER_MODE", "local")
	t.Setenv("TILLER_DATA_DIR", t.TempDir())
	t.Setenv("TILLER_USERNAME", "")
	t.Setenv("TILLER_PASSWORD", "")
	t.Setenv("TILLER_ADMIN_USERNAME", "")
	t.Setenv("TILLER_ADMIN_PASSWORD", "")
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{"", []string{"1.1.1.1:53", "1.0.0.1:53", "8.8.8.8:53", "8.8.4.4:53"}},
		{"off", nil},
		{"192.168.1.1,10.0.0.1:5353", []string{"192.168.1.1:53", "10.0.0.1:5353"}},
	} {
		t.Setenv("TILLER_DNS_FALLBACK_SERVERS", tc.raw)
		cfg, err := Load()
		if err != nil || !reflect.DeepEqual(cfg.DNSFallbackServers, tc.want) {
			t.Fatalf("DNS config %q = %v, %v; want %v", tc.raw, cfg.DNSFallbackServers, err, tc.want)
		}
	}
	t.Setenv("TILLER_DNS_FALLBACK_SERVERS", "dns.example")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "TILLER_DNS_FALLBACK_SERVERS") {
		t.Fatalf("invalid DNS config = %v", err)
	}
}
