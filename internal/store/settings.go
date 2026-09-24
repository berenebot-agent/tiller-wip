package store

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

// GetSetting returns the raw string value for an account settings key. Secret
// settings are decrypted transparently.
func (s *Scope) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.q.QueryRowContext(ctx, `SELECT value FROM settings WHERE account_id=? AND key=?`, s.accountID, key).Scan(&value)
	if err != nil {
		return "", err
	}
	if secretSettingKey(key) {
		return s.decryptSecret(secretAAD(s.accountID, "setting", key, "value"), value)
	}
	return value, nil
}

// SetSetting upserts an account settings key. Secret settings are encrypted
// before they are written.
func (s *Scope) SetSetting(ctx context.Context, key, value string) error {
	stored := value
	if secretSettingKey(key) {
		enc, err := s.encryptSecret(secretAAD(s.accountID, "setting", key, "value"), value)
		if err != nil {
			return err
		}
		stored = enc
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO settings(account_id,key,value,updated_at) VALUES(?,?,?,?)
ON CONFLICT(account_id,key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		s.accountID, key, stored, now())
	return err
}

// secretSettingKey reports whether a settings key holds a recoverable secret.
func secretSettingKey(key string) bool {
	for _, k := range secretSettingKeys() {
		if k == key {
			return true
		}
	}
	return false
}

func (s *Scope) GetBool(ctx context.Context, key string) (bool, error) {
	value, err := s.GetSetting(ctx, key)
	if err != nil {
		return false, err
	}
	return strconv.ParseBool(value)
}

func (s *Scope) GetInt(ctx context.Context, key string) (int, error) {
	value, err := s.GetSetting(ctx, key)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(value)
}

// GetLoggingDefaults returns the account defaults for new client keys.
func (s *Scope) GetLoggingDefaults(ctx context.Context) (enabled bool, retentionDays int, err error) {
	enabled = true
	retentionDays = 30
	if v, e := s.GetBool(ctx, SettingDefaultLoggingEnabled); e == nil {
		enabled = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return false, 0, e
	}
	if v, e := s.GetInt(ctx, SettingDefaultRetentionDays); e == nil {
		retentionDays = v
	} else if !errors.Is(e, sql.ErrNoRows) {
		return false, 0, e
	}
	return enabled, retentionDays, nil
}

// GetLogErrorBodies returns whether failed request and upstream error bodies
// should be retained. The safe default is disabled.
func (s *Scope) GetLogErrorBodies(ctx context.Context) (bool, error) {
	v, err := s.GetBool(ctx, SettingLogErrorBodies)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v, err
}

// GetFallbackTimeout returns the configured fallback timeout in seconds.
func (s *Scope) GetFallbackTimeout(ctx context.Context) (int, error) {
	const fallback = 60
	if v, e := s.GetInt(ctx, SettingFallbackTimeoutSeconds); e == nil {
		return v, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return 0, e
	}
	return fallback, nil
}

// GetFallbackCooldownSeconds returns the configured fallback cooldown in
// seconds; 0 disables the cooldown feature.
func (s *Scope) GetFallbackCooldownSeconds(ctx context.Context) (int, error) {
	const fallback = 300
	if v, e := s.GetInt(ctx, SettingFallbackCooldownSeconds); e == nil {
		return v, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return 0, e
	}
	return fallback, nil
}

// NotificationSettings holds the account's outbound webhook configuration.
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

// GetNotificationSettings reads the account notification configuration.
func (s *Scope) GetNotificationSettings(ctx context.Context) (NotificationSettings, error) {
	return s.GetNotificationSettingsBatch(ctx)
}

func (s *Scope) GetNotificationSettingsBatch(ctx context.Context) (NotificationSettings, error) {
	ns := NotificationSettings{EventFallback: true, EventAllFailed: true, CooldownSeconds: 60, EventAdminLogin: true}
	keys := []string{SettingNotificationsEnabled, SettingNotificationsWebhookURL, SettingNotificationsEventFallback, SettingNotificationsEventAllFailed, SettingNotificationsAuthHeader, SettingNotificationsCooldownSeconds, SettingNotificationsEventClientKeyCreated, SettingNotificationsEventClientKeyDeleted, SettingNotificationsEventAdminLogin}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys)+1)
	args = append(args, s.accountID)
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err := s.q.QueryContext(ctx, `SELECT key,value FROM settings WHERE account_id=? AND key IN (`+placeholders+`)`, args...)
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
		decrypted, err := s.decryptSecret(secretAAD(s.accountID, "setting", SettingNotificationsAuthHeader, "value"), value)
		if err != nil {
			return ns, err
		}
		ns.AuthHeader = decrypted
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
