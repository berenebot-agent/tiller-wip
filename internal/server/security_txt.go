package server

import (
	"net/http"
	"strings"
	"time"
)

// securityTxt is the RFC 9116 security-contact file, served for both
// /security.txt and /.well-known/security.txt. It is the same in every mode.
//
// Expires is required by RFC 9116 and must not be stale, so it is computed at
// serve time (one year ahead). Canonical is built from the configured public
// URL and omitted when none is set, since it must be an absolute https URI.
//
// The Policy field points at the public repository's SECURITY.md, which is the
// canonical disclosure policy.
const securityTxtContact = "tiller-router@hgolabs.com"

func (s *Server) securityTxtBody(now time.Time) string {
	body := "Contact: mailto:" + securityTxtContact + "\r\n" +
		"Expires: " + now.UTC().AddDate(1, 0, 0).Format("2006-01-02T15:04:05.000Z") + "\r\n" +
		"Preferred-Languages: en\r\n"
	if base := strings.TrimRight(s.config.PublicURL, "/"); strings.HasPrefix(base, "https://") {
		body += "Canonical: " + base + "/.well-known/security.txt\r\n"
	}
	return body + "Policy: https://github.com/dellarb/tiller-router/blob/master/SECURITY.md\r\n"
}

// handleSecurityTxt serves the security contact file as plain text.
func (s *Server) handleSecurityTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.securityTxtBody(time.Now())))
}
