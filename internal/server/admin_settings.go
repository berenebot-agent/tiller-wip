package server

import (
	"net/http"
	"net/url"
	"reflect"
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
	logErrorBodies, err := s.db.GetLogErrorBodies(r.Context())
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
		"log_error_bodies":                       logErrorBodies,
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
		LogErrorBodies                     *bool   `json:"log_error_bodies"`
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
	// Each entry writes its setting only when the field was supplied (non-nil),
	// so a PATCH touches exactly the fields present. The auth header is a
	// secret: it is never returned by GET; a non-nil value here replaces it
	// (empty string clears it) and a nil value leaves it unchanged.
	type settingUpdate struct {
		value any // *bool / *int / *string; nil skips the write
		key   string
	}
	updates := []settingUpdate{
		{key: database.SettingDefaultLoggingEnabled, value: input.DefaultLoggingEnabled},
		{key: database.SettingDefaultRetentionDays, value: input.DefaultRetentionDays},
		{key: database.SettingLogErrorBodies, value: input.LogErrorBodies},
		{key: database.SettingFallbackTimeoutSeconds, value: input.FallbackTimeoutSeconds},
		{key: database.SettingNotificationsEnabled, value: input.NotificationsEnabled},
		{key: database.SettingNotificationsWebhookURL, value: input.NotificationsWebhookURL},
		{key: database.SettingNotificationsEventFallback, value: input.NotificationsEventFallback},
		{key: database.SettingNotificationsEventAllFailed, value: input.NotificationsEventAllFailed},
		{key: database.SettingNotificationsCooldownSeconds, value: input.NotificationsCooldownSeconds},
		{key: database.SettingNotificationsEventClientKeyCreated, value: input.NotificationsEventClientKeyCreated},
		{key: database.SettingNotificationsEventClientKeyDeleted, value: input.NotificationsEventClientKeyDeleted},
		{key: database.SettingNotificationsEventAdminLogin, value: input.NotificationsEventAdminLogin},
		{key: database.SettingNotificationsAuthHeader, value: input.NotificationsAuthHeader},
	}
	for _, u := range updates {
		if u.value == nil || reflect.ValueOf(u.value).IsNil() {
			continue
		}
		var value string
		switch v := u.value.(type) {
		case *bool:
			value = strconv.FormatBool(*v)
		case *int:
			value = strconv.Itoa(*v)
		case *string:
			value = *v
		}
		if err := s.db.SetSetting(r.Context(), u.key, value); err != nil {
			adminError(w, 500, "database_error", "Could not update settings.")
			return
		}
		if u.key == database.SettingFallbackTimeoutSeconds {
			s.providers.Registry().SetResponseHeaderTimeout(time.Duration(*input.FallbackTimeoutSeconds) * time.Second)
		}
	}
	w.WriteHeader(204)
}

// validWebhookURL reports whether a webhook URL is an absolute http(s) URL.
func validWebhookURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
