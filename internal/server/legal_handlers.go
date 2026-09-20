package server

import "net/http"

// WS2 — legal handlers. Own this file.
// Implements the endpoint contract in docs/stage_d_api_contract.md.

// legalDoc serves a current published legal document publicly (signup links it
// before authentication).
func (s *Server) legalDoc(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}

// writePlatformLegalDocs lists documents for the platform editor.
func (s *Server) writePlatformLegalDocs(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}

// writePlatformLegalUpdate publishes a document.
func (s *Server) writePlatformLegalUpdate(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}
