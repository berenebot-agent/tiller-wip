package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const (
	PlatformSettingHostedSignupEnabled = "hosted_signup_enabled"
	PlatformSettingMailProvider        = "mail_provider"
	PlatformSettingMailFrom            = "mail_from"
	PlatformSettingMailResendAPIKey    = "mail_resend_api_key"
	PlatformSettingMailSMTPHost        = "mail_smtp_host"
	PlatformSettingMailSMTPPort        = "mail_smtp_port"
	PlatformSettingMailSMTPUsername    = "mail_smtp_username"
	PlatformSettingMailSMTPPassword    = "mail_smtp_password"
	PlatformSettingMailSMTPMode        = "mail_smtp_mode"
)

var platformSecretSettings = map[string]bool{
	PlatformSettingMailResendAPIKey: true,
	PlatformSettingMailSMTPPassword: true,
}

// GetPlatformSetting reads a platform setting and decrypts mail secrets at the
// store boundary. The admin credential fingerprint is intentionally not
// exposed through this helper.
func (s *Store) GetPlatformSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM platform_settings WHERE key=?`, key).Scan(&value)
	if err != nil {
		return "", err
	}
	if platformSecretSettings[key] {
		return decryptWith(s.cipher, secretAAD("platform", "setting", key, "value"), value)
	}
	return value, nil
}

// SetPlatformSetting writes a platform setting. Recoverable mail secrets are
// encrypted before persistence; an empty secret clears the row value.
func (s *Store) SetPlatformSetting(ctx context.Context, key, value string) error {
	stored := value
	if platformSecretSettings[key] {
		if value == "" {
			stored = ""
		} else {
			enc, err := encryptWith(s.cipher, secretAAD("platform", "setting", key, "value"), value)
			if err != nil {
				return err
			}
			stored = enc
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO platform_settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, stored, now())
	return err
}

func (s *Store) DeletePlatformSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM platform_settings WHERE key=?`, key)
	return err
}

// PlatformMailSettings is the decrypted in-memory mail configuration. It is
// never serialized directly to an HTTP response.
type PlatformMailSettings struct {
	Provider     string
	From         string
	ResendAPIKey string
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPMode     string
}

func (s *Store) GetPlatformMailSettings(ctx context.Context) (PlatformMailSettings, error) {
	keys := []string{
		PlatformSettingMailProvider, PlatformSettingMailFrom, PlatformSettingMailResendAPIKey,
		PlatformSettingMailSMTPHost, PlatformSettingMailSMTPPort, PlatformSettingMailSMTPUsername,
		PlatformSettingMailSMTPPassword, PlatformSettingMailSMTPMode,
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM platform_settings WHERE key IN (`+placeholders+`)`, args...)
	if err != nil {
		return PlatformMailSettings{}, err
	}
	defer rows.Close()
	values := make(map[string]string, len(keys))
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return PlatformMailSettings{}, err
		}
		if platformSecretSettings[key] {
			value, err = decryptWith(s.cipher, secretAAD("platform", "setting", key, "value"), value)
			if err != nil {
				return PlatformMailSettings{}, err
			}
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return PlatformMailSettings{}, err
	}
	return PlatformMailSettings{
		Provider: values[PlatformSettingMailProvider], From: values[PlatformSettingMailFrom],
		ResendAPIKey: values[PlatformSettingMailResendAPIKey], SMTPHost: values[PlatformSettingMailSMTPHost],
		SMTPPort: values[PlatformSettingMailSMTPPort], SMTPUsername: values[PlatformSettingMailSMTPUsername],
		SMTPPassword: values[PlatformSettingMailSMTPPassword], SMTPMode: values[PlatformSettingMailSMTPMode],
	}, nil
}

// SeedPlatformMailSettings writes only keys that are currently absent. It is
// used for one-time environment bootstrap; dashboard/database values win after
// the first seed.
func (s *Store) SeedPlatformMailSettings(ctx context.Context, settings PlatformMailSettings) error {
	values := map[string]string{
		PlatformSettingMailProvider: settings.Provider, PlatformSettingMailFrom: settings.From,
		PlatformSettingMailResendAPIKey: settings.ResendAPIKey, PlatformSettingMailSMTPHost: settings.SMTPHost,
		PlatformSettingMailSMTPPort: settings.SMTPPort, PlatformSettingMailSMTPUsername: settings.SMTPUsername,
		PlatformSettingMailSMTPPassword: settings.SMTPPassword, PlatformSettingMailSMTPMode: settings.SMTPMode,
	}
	for key, value := range values {
		if value == "" {
			continue
		}
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM platform_settings WHERE key=?`, key).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			continue
		}
		if err := s.SetPlatformSetting(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) HostedSignupEnabled(ctx context.Context) (bool, error) {
	value, err := s.GetPlatformSetting(ctx, PlatformSettingHostedSignupEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == "1" || strings.EqualFold(value, "true"), nil
}

func (s *Store) SetHostedSignupEnabled(ctx context.Context, enabled bool) error {
	value := "0"
	if enabled {
		value = "1"
	}
	return s.SetPlatformSetting(ctx, PlatformSettingHostedSignupEnabled, value)
}
