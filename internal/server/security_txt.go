package server

import "net/http"

// securityTxt is the RFC 9116 security-contact file. It is served for both
// /security.txt and /.well-known/security.txt (and at the hosted origin via the
// app). Placeholder tokens in the embedded template are replaced at serve time
// from configuration so the published file names the real operator contact once
// it is set.
const securityTxtBody = "Contact: mailto:security@tillerrouter.com\r\n" +
	"Preferred-Languages: en\r\n" +
	"Canonical: /security.txt\r\n" +
	"Policy: /legal/security\r\n"

// writeSecurityTxt serves the security contact file as plain text.
func (s *Server) handleSecurityTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(securityTxtBody))
}
