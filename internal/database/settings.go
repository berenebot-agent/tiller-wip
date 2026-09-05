package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

const (
	SettingDefaultLoggingEnabled              = "default_logging_enabled"
	SettingDefaultRetentionDays               = "default_retention_days"
	SettingLogErrorBodies                     = "log_error_bodies"
	SettingFallbackTimeoutSeconds             = "fallback_timeout_seconds"
	SettingNotificationsEnabled               = "notifications_enabled"
	SettingNotificationsWebhookURL            = "notifications_webhook_url"
	SettingNotificationsEventFallback         = "notifications_event_fallback"
	SettingNotificationsEventAllFailed        = "notifications_event_all_failed"
	SettingNotificationsAuthHeader            = "notifications_auth_header"
	SettingNotificationsCooldownSeconds       = "notifications_cooldown_seconds"
	SettingNotificationsEventClientKeyCreated = "notifications_event_client_key_created"
	SettingNotificationsEventClientKeyDeleted = "notifications_event_client_key_deleted"
	SettingNotificationsEventAdminLogin       = "notifications_event_admin_login"
)

// GetSetting returns the raw string value for a settings key.
func (d *DB) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := d.SQL.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	return value, err
}

// SetSetting upserts a settings key.
func (d *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := d.SQL.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, Now())
	return err
}

// GetBool reads a settings key as a boolean.
func (d *DB) GetBool(ctx context.Context, key string) (bool, error) {
	value, err := d.GetSetting(ctx, key)
	if err != nil {
		return false, err
	}
	return strconv.ParseBool(value)
}

// GetInt reads a settings key as an integer.
func (d *DB) GetInt(ctx context.Context, key string) (int, error) {
	value, err := d.GetSetting(ctx, key)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(value)
}

// GetLoggingDefaults returns the global defaults for new client keys, with
// sane fallbacks if a key is missing or malformed.
func (d *DB) GetLoggingDefaults(ctx context.Context) (enabled bool, retentionDays int, err error) {
	enabled = true
	retentionDays = 30
	if v, e := d.GetBool(ctx, SettingDefaultLoggingEnabled); e == nil {
		enabled = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return false, 0, e
	}
	if v, e := d.GetInt(ctx, SettingDefaultRetentionDays); e == nil {
		retentionDays = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return false, 0, e
	}
	return enabled, retentionDays, nil
}

// GetLogErrorBodies returns whether failed request and upstream error bodies
// should be retained. The safe default is disabled.
func (d *DB) GetLogErrorBodies(ctx context.Context) (bool, error) {
	v, err := d.GetBool(ctx, SettingLogErrorBodies)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v, err
}

// GetFallbackTimeout returns the configured fallback timeout in seconds, with a
// sane default of 60 if the key is missing or malformed.
func (d *DB) GetFallbackTimeout(ctx context.Context) (int, error) {
	const fallback = 60
	if v, e := d.GetInt(ctx, SettingFallbackTimeoutSeconds); e == nil {
		return v, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return 0, e
	}
	return fallback, nil
}

// NotificationSettings holds the installation-global outbound webhook
// notification configuration. Event toggles default to enabled so a configured
// webhook starts notifying immediately. CooldownSeconds defaults to 60 so repeat
// alerts for the same event + model are throttled to one per minute.
type NotificationSettings struct {
	Enabled               bool
	WebhookURL            string
	EventFallback         bool
	EventAllFailed        bool
	AuthHeader            string
	CooldownSeconds       int
	EventClientKeyCreated bool
	EventClientKeyDeleted bool
	EventAdminLogin       bool
}

// GetNotificationSettings reads the notification configuration, with sane
// defaults if a key is missing or malformed.
func (d *DB) GetNotificationSettings(ctx context.Context) (NotificationSettings, error) {
	ns := NotificationSettings{EventFallback: true, EventAllFailed: true, CooldownSeconds: 60, EventAdminLogin: true}
	if v, e := d.GetBool(ctx, SettingNotificationsEnabled); e == nil {
		ns.Enabled = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetSetting(ctx, SettingNotificationsWebhookURL); e == nil {
		ns.WebhookURL = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetBool(ctx, SettingNotificationsEventFallback); e == nil {
		ns.EventFallback = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetBool(ctx, SettingNotificationsEventAllFailed); e == nil {
		ns.EventAllFailed = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetSetting(ctx, SettingNotificationsAuthHeader); e == nil {
		ns.AuthHeader = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetInt(ctx, SettingNotificationsCooldownSeconds); e == nil {
		ns.CooldownSeconds = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetBool(ctx, SettingNotificationsEventClientKeyCreated); e == nil {
		ns.EventClientKeyCreated = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetBool(ctx, SettingNotificationsEventClientKeyDeleted); e == nil {
		ns.EventClientKeyDeleted = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	if v, e := d.GetBool(ctx, SettingNotificationsEventAdminLogin); e == nil {
		ns.EventAdminLogin = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return ns, e
	}
	return ns, nil
}
