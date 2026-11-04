package server

import (
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
)

// TestSecurityTxtIsValidRFC9116 proves the published file carries the required
// Expires field, the real contact, and an absolute Canonical built from the
// public URL (omitted when none is configured).
func TestSecurityTxtIsValidRFC9116(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	hosted := (&Server{config: config.Config{PublicURL: "https://tiller.example.com/"}}).securityTxtBody(now)
	for _, want := range []string{
		"Contact: mailto:tiller-router@hgolabs.com\r\n",
		"Expires: 2027-11-04T12:00:00.000Z\r\n",
		"Canonical: https://tiller.example.com/.well-known/security.txt\r\n",
	} {
		if !strings.Contains(hosted, want) {
			t.Errorf("security.txt missing %q:\n%s", want, hosted)
		}
	}
	local := (&Server{config: config.Config{}}).securityTxtBody(now)
	if strings.Contains(local, "Canonical:") || strings.Contains(local, "tillerrouter.com") {
		t.Errorf("local security.txt has a Canonical or the old contact:\n%s", local)
	}
}
