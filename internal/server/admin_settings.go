package server

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	enabled, retention, err := s.db.GetLoggingDefaults(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not load settings.")
		return
	}
	fallbackTimeout, err := s.db.GetFallbackTimeout(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not load settings.")
		return
	}
	notifications, err := s.db.GetNotificationSettings(r.Context())
	if err != nil {
		adminError(w, 500, "database_error", "Could not load settings.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"default_logging_enabled":                enabled,
		"default_retention_days":                 retention,
		"fallback_timeout_seconds":               fallbackTimeout,
		"notifications_enabled":                  notifications.Enabled,
		"notifications_webhook_url":              notifications.WebhookURL,
		"notifications_event_fallback":           notifications.EventFallback,
		"notifications_event_all_failed":         notifications.EventAllFailed,
		"notifications_cooldown_seconds":         notifications.CooldownSeconds,
		"notifications_event_client_key_created": notifications.EventClientKeyCreated,
		"notifications_event_client_key_deleted": notifications.EventClientKeyDeleted,
		"notifications_event_admin_login":        notifications.EventAdminLogin,
		"notifications_auth_header_set":          notifications.AuthHeader != "",
	})
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var input struct {
		DefaultLoggingEnabled              *bool   `json:"default_logging_enabled"`
		DefaultRetentionDays               *int    `json:"default_retention_days"`
		FallbackTimeoutSeconds             *int    `json:"fallback_timeout_seconds"`
		NotificationsEnabled               *bool   `json:"notifications_enabled"`
		NotificationsWebhookURL            *string `json:"notifications_webhook_url"`
		NotificationsEventFallback         *bool   `json:"notifications_event_fallback"`
		NotificationsEventAllFailed        *bool   `json:"notifications_event_all_failed"`
		NotificationsCooldownSeconds       *int    `json:"notifications_cooldown_seconds"`
		NotificationsEventClientKeyCreated *bool   `json:"notifications_event_client_key_created"`
		NotificationsEventClientKeyDeleted *bool   `json:"notifications_event_client_key_deleted"`
		NotificationsEventAdminLogin       *bool   `json:"notifications_event_admin_login"`
		NotificationsAuthHeader            *string `json:"notifications_auth_header"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		adminError(w, 400, "invalid_request", err.Error())
		return
	}
	if input.DefaultRetentionDays != nil && *input.DefaultRetentionDays < 1 {
		adminError(w, 400, "invalid_retention", "Retention must be at least 1 day.")
		return
	}
	if input.FallbackTimeoutSeconds != nil && (*input.FallbackTimeoutSeconds < 1 || *input.FallbackTimeoutSeconds > 3600) {
		adminError(w, 400, "invalid_fallback_timeout", "Fallback timeout must be between 1 and 3600 seconds.")
		return
	}
	if input.NotificationsWebhookURL != nil && *input.NotificationsWebhookURL != "" {
		if !validWebhookURL(*input.NotificationsWebhookURL) {
			adminError(w, 400, "invalid_webhook_url", "The webhook URL must be a valid http(s) URL.")
			return
		}
	}
	if input.NotificationsCooldownSeconds != nil && *input.NotificationsCooldownSeconds < 0 {
		adminError(w, 400, "invalid_cooldown", "Notification cooldown must be 0 or more seconds.")
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
	if input.FallbackTimeoutSeconds != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingFallbackTimeoutSeconds, strconv.Itoa(*input.FallbackTimeoutSeconds)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
		s.providers.Registry().SetResponseHeaderTimeout(time.Duration(*input.FallbackTimeoutSeconds) * time.Second)
	}
	if input.NotificationsEnabled != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsEnabled, strconv.FormatBool(*input.NotificationsEnabled)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.NotificationsWebhookURL != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsWebhookURL, *input.NotificationsWebhookURL); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.NotificationsEventFallback != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsEventFallback, strconv.FormatBool(*input.NotificationsEventFallback)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.NotificationsEventAllFailed != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsEventAllFailed, strconv.FormatBool(*input.NotificationsEventAllFailed)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.NotificationsCooldownSeconds != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsCooldownSeconds, strconv.Itoa(*input.NotificationsCooldownSeconds)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.NotificationsEventClientKeyCreated != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsEventClientKeyCreated, strconv.FormatBool(*input.NotificationsEventClientKeyCreated)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.NotificationsEventClientKeyDeleted != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsEventClientKeyDeleted, strconv.FormatBool(*input.NotificationsEventClientKeyDeleted)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	if input.NotificationsEventAdminLogin != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsEventAdminLogin, strconv.FormatBool(*input.NotificationsEventAdminLogin)); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	// The auth header is a secret: it is never returned by GET. A non-nil value
	// here replaces it (empty string clears it); a nil value leaves it unchanged.
	if input.NotificationsAuthHeader != nil {
		if err := s.db.SetSetting(r.Context(), database.SettingNotificationsAuthHeader, *input.NotificationsAuthHeader); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
	}
	w.WriteHeader(204)
}

// validWebhookURL reports whether a webhook URL is an absolute http(s) URL.
func validWebhookURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
