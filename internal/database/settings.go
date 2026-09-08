package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
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
	SettingFallbackCooldownSeconds            = "fallback_cooldown_seconds"
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

// GetFallbackCooldownSeconds returns the configured fallback cooldown in seconds,
// with a sane default of 300 (5 minutes) if the key is missing or malformed.
// A value of 0 disables the cooldown feature.
func (d *DB) GetFallbackCooldownSeconds(ctx context.Context) (int, error) {
	const fallback = 300
	if v, e := d.GetInt(ctx, SettingFallbackCooldownSeconds); e == nil {
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
	return d.GetNotificationSettingsBatch(ctx)
}

func (d *DB) GetNotificationSettingsBatch(ctx context.Context) (NotificationSettings, error) {
	ns := NotificationSettings{EventFallback: true, EventAllFailed: true, CooldownSeconds: 60, EventAdminLogin: true}
	keys := []string{SettingNotificationsEnabled, SettingNotificationsWebhookURL, SettingNotificationsEventFallback, SettingNotificationsEventAllFailed, SettingNotificationsAuthHeader, SettingNotificationsCooldownSeconds, SettingNotificationsEventClientKeyCreated, SettingNotificationsEventClientKeyDeleted, SettingNotificationsEventAdminLogin}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	rows, err := d.SQL.QueryContext(ctx, `SELECT key,value FROM settings WHERE key IN (`+placeholders+`)`, stringArgs(keys)...)
	if err != nil {
		return ns, err
	}
	defer rows.Close()
	values := make(map[string]string, len(keys))
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return ns, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return ns, err
	}
	parseBool := func(key string, target *bool) error {
		value, ok := values[key]
		if !ok {
			return nil
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		*target = parsed
		return nil
	}
	if err := parseBool(SettingNotificationsEnabled, &ns.Enabled); err != nil {
		return ns, err
	}
	if value, ok := values[SettingNotificationsWebhookURL]; ok {
		ns.WebhookURL = value
	}
	if err := parseBool(SettingNotificationsEventFallback, &ns.EventFallback); err != nil {
		return ns, err
	}
	if err := parseBool(SettingNotificationsEventAllFailed, &ns.EventAllFailed); err != nil {
		return ns, err
	}
	if value, ok := values[SettingNotificationsAuthHeader]; ok {
		ns.AuthHeader = value
	}
	if value, ok := values[SettingNotificationsCooldownSeconds]; ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return ns, err
		}
		ns.CooldownSeconds = parsed
	}
	if err := parseBool(SettingNotificationsEventClientKeyCreated, &ns.EventClientKeyCreated); err != nil {
		return ns, err
	}
	if err := parseBool(SettingNotificationsEventClientKeyDeleted, &ns.EventClientKeyDeleted); err != nil {
		return ns, err
	}
	if err := parseBool(SettingNotificationsEventAdminLogin, &ns.EventAdminLogin); err != nil {
		return ns, err
	}
	return ns, nil
}

func stringArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}
