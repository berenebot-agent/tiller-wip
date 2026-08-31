package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

const (
	SettingDefaultLoggingEnabled  = "default_logging_enabled"
	SettingDefaultRetentionDays   = "default_retention_days"
	SettingFallbackTimeoutSeconds = "fallback_timeout_seconds"
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
