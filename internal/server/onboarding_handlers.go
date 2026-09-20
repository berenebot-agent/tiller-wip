package server

import "net/http"

// WS3 — onboarding + export handlers. Own this file.
// Implements the endpoint contract in docs/stage_d_api_contract.md.

// writeOnboardingState reports whether the first-run wizard should be shown.
func (s *Server) writeOnboardingState(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}

// setOnboardingDismissed remembers that the user skipped setup.
func (s *Server) setOnboardingDismissed(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}

// writeAccountExport streams the account-scoped ZIP export.
func (s *Server) writeAccountExport(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}
