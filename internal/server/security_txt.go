package server

import "net/http"

// securityTxt is the RFC 9116 security-contact file. It is served for both
// /security.txt and /.well-known/security.txt (and at the hosted origin via the
// app). Placeholder tokens in the embedded template are replaced at serve time
// from configuration so the published file names the real operator contact once
// it is set.
//
// The Policy field points at the public repository's SECURITY.md. Hosted Tiller
// no longer publishes a separate security policy document, so the repository's
// public security policy is the canonical disclosure.
const securityTxtBody = "Contact: mailto:security@tillerrouter.com\r\n" +
	"Preferred-Languages: en\r\n" +
	"Canonical: /security.txt\r\n" +
	"Policy: https://github.com/dellarb/tiller-router/blob/master/SECURITY.md\r\n"

// writeSecurityTxt serves the security contact file as plain text.
func (s *Server) handleSecurityTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(securityTxtBody))
}
