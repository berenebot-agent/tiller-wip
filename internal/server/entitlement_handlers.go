package server

import "net/http"

// WS1 — entitlement / plan handlers. Own this file.
// Implements the endpoint contract in docs/stage_d_api_contract.md.

// writePlatformPlans lists the plan catalogue for the platform dashboard.
func (s *Server) writePlatformPlans(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}

// writePlatformPlanUpdate replaces the caps of one plan.
func (s *Server) writePlatformPlanUpdate(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}

// writeAccountPlan returns the caller's plan caps and current usage counts.
func (s *Server) writeAccountPlan(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}

// writeAccountPlanAssign moves an account onto a plan (operator action).
func (s *Server) writeAccountPlanAssign(w http.ResponseWriter, r *http.Request) {
	adminError(w, http.StatusNotImplemented, "not_implemented", "Not implemented.")
}
