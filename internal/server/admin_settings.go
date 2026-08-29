package server

import (
	"net/http"
	"strconv"

	"github.com/tiller-router/tiller-router/internal/database"
)

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	enabled, retention, err := s.db.GetLoggingDefaults(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not load settings.")
		return
	}
	writeJSON(w, 200, map[string]any{"default_logging_enabled": enabled, "default_retention_days": retention})
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DefaultLoggingEnabled *bool `json:"default_logging_enabled"`
		DefaultRetentionDays  *int  `json:"default_retention_days"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	if input.DefaultRetentionDays != nil && *input.DefaultRetentionDays < 1 {
		adminError(w, 400, "invalid_retention", "Retention must be at least 1 day.")
		return
	}
	if input.DefaultLoggingEnabled != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingDefaultLoggingEnabled, strconv.FormatBool(*input.DefaultLoggingEnabled)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.DefaultRetentionDays != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingDefaultRetentionDays, strconv.Itoa(*input.DefaultRetentionDays)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	w.WriteHeader(204)
}
