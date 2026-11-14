package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

const (
	PlatformSettingHostedSignupEnabled   = "hosted_signup_enabled"
	PlatformSettingMailProvider          = "mail_provider"
	PlatformSettingMailFrom              = "mail_from"
	PlatformSettingMailFromName          = "mail_from_name"
	PlatformSettingMailResendAPIKey      = "mail_resend_api_key"
	PlatformSettingMailBrevoAPIKey       = "mail_brevo_api_key"
	PlatformSettingMailSMTPHost          = "mail_smtp_host"
	PlatformSettingMailSMTPPort          = "mail_smtp_port"
	PlatformSettingMailSMTPUsername      = "mail_smtp_username"
	PlatformSettingMailSMTPPassword      = "mail_smtp_password"
	PlatformSettingMailSMTPMode          = "mail_smtp_mode"
	PlatformSettingGoogleEnabled         = "google_signin_enabled"
	PlatformSettingGoogleClientID        = "google_client_id"
	PlatformSettingGoogleClientSecret    = "google_client_secret"
	PlatformSettingTurnstileEnabled      = "turnstile_enabled"
	PlatformSettingTurnstileSiteKey      = "turnstile_site_key"
	PlatformSettingTurnstileSecret       = "turnstile_secret"
	PlatformSettingAnalyticsEnabled      = "analytics_enabled"
	PlatformSettingAnalyticsProvider     = "analytics_provider"
	PlatformSettingAnalyticsScriptURL    = "analytics_script_url"
	PlatformSettingAnalyticsSiteID       = "analytics_site_id"
	PlatformSettingPreventSearchIndexing = "prevent_search_indexing"
)

// SecretUnchanged is a sentinel meaning "leave the stored value alone". It is
// used when a secret could not be read (locked cipher) so a save cannot erase
// it. It is never persisted.
const SecretUnchanged = "\x00secret-unchanged"

var platformSecretSettings = map[string]bool{
	PlatformSettingMailResendAPIKey:   true,
	PlatformSettingMailBrevoAPIKey:    true,
	PlatformSettingMailSMTPPassword:   true,
	PlatformSettingGoogleClientSecret: true,
	PlatformSettingTurnstileSecret:    true,
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
	Provider string
	From     string
	// FromName is the optional display name shown in the recipient's mailbox.
	// It is public (not a secret) and is never part of the envelope address.
	FromName     string
	ResendAPIKey string
	BrevoAPIKey  string
	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string
	SMTPMode     string
}

type PlatformSettingsProposal struct {
	HostedSignupEnabled   bool
	AuditRetentionDays    int
	PreventSearchIndexing bool
	Mail                  PlatformMailSettings
	Auth                  PlatformAuthSettings
	Analytics             PlatformAnalyticsSettings
}

// PlatformAnalyticsSettings is the operator-configured, consent-gated web
// analytics integration for hosted mode. Values are public (they are served to
// every hosted page), so they are stored in plaintext and require no cipher.
type PlatformAnalyticsSettings struct {
	Enabled   bool
	Provider  string
	ScriptURL string
	SiteID    string
}

// PlatformAuthSettings contains hosted-only login integration settings.
// Secret values are decrypted only inside the server and never serialized.
type PlatformAuthSettings struct {
	GoogleEnabled      bool
	GoogleClientID     string
	GoogleClientSecret string
	TurnstileEnabled   bool
	TurnstileSiteKey   string
	TurnstileSecret    string
}

func (s *Store) SavePlatformSettings(ctx context.Context, proposal PlatformSettingsProposal) error {
	if proposal.AuditRetentionDays < 1 {
		return errors.New("store: invalid audit retention")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	upsert := func(key, value string) error {
		if platformSecretSettings[key] && value != "" {
			value, err = encryptWith(s.cipher, secretAAD("platform", "setting", key, "value"), value)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO platform_settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, now())
		return err
	}
	if err := upsert(PlatformSettingHostedSignupEnabled, boolString(proposal.HostedSignupEnabled)); err != nil {
		return err
	}
	if err := upsert(PlatformSettingPreventSearchIndexing, boolString(proposal.PreventSearchIndexing)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_meta(key,value,updated_at) VALUES('audit_retention_days',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, strconv.Itoa(proposal.AuditRetentionDays), now()); err != nil {
		return err
	}
	values := map[string]string{
		PlatformSettingMailProvider: proposal.Mail.Provider, PlatformSettingMailFrom: proposal.Mail.From,
		PlatformSettingMailFromName:     proposal.Mail.FromName,
		PlatformSettingMailResendAPIKey: proposal.Mail.ResendAPIKey, PlatformSettingMailBrevoAPIKey: proposal.Mail.BrevoAPIKey,
		PlatformSettingMailSMTPHost: proposal.Mail.SMTPHost, PlatformSettingMailSMTPPort: proposal.Mail.SMTPPort,
		PlatformSettingMailSMTPUsername: proposal.Mail.SMTPUsername, PlatformSettingMailSMTPPassword: proposal.Mail.SMTPPassword,
		PlatformSettingMailSMTPMode:       proposal.Mail.SMTPMode,
		PlatformSettingGoogleEnabled:      boolString(proposal.Auth.GoogleEnabled),
		PlatformSettingGoogleClientID:     proposal.Auth.GoogleClientID,
		PlatformSettingGoogleClientSecret: proposal.Auth.GoogleClientSecret,
		PlatformSettingTurnstileEnabled:   boolString(proposal.Auth.TurnstileEnabled),
		PlatformSettingTurnstileSiteKey:   proposal.Auth.TurnstileSiteKey,
		PlatformSettingTurnstileSecret:    proposal.Auth.TurnstileSecret,
		PlatformSettingAnalyticsEnabled:   boolString(proposal.Analytics.Enabled),
		PlatformSettingAnalyticsProvider:  proposal.Analytics.Provider,
		PlatformSettingAnalyticsScriptURL: proposal.Analytics.ScriptURL,
		PlatformSettingAnalyticsSiteID:    proposal.Analytics.SiteID,
	}
	for key, value := range values {
		if value == SecretUnchanged {
			continue
		}
		if err := upsert(key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetPlatformPreventSearchIndexing reports whether hosted responses should
// carry a no-index directive. Missing rows default to false for existing
// installations.
func (s *Store) GetPlatformPreventSearchIndexing(ctx context.Context) (bool, error) {
	value, err := s.GetPlatformSetting(ctx, PlatformSettingPreventSearchIndexing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return value == "1" || strings.EqualFold(value, "true"), nil
}

// GetPlatformAuthSettings reads hosted sign-in and CAPTCHA settings. Missing
// rows use disabled/empty defaults. Secrets are decrypted at this boundary.
func (s *Store) GetPlatformAuthSettings(ctx context.Context) (PlatformAuthSettings, error) {
	keys := []string{
		PlatformSettingGoogleEnabled, PlatformSettingGoogleClientID, PlatformSettingGoogleClientSecret,
		PlatformSettingTurnstileEnabled, PlatformSettingTurnstileSiteKey, PlatformSettingTurnstileSecret,
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM platform_settings WHERE key IN (`+placeholders+`)`, args...)
	if err != nil {
		return PlatformAuthSettings{}, err
	}
	defer rows.Close()
	values := make(map[string]string, len(keys))
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return PlatformAuthSettings{}, err
		}
		if platformSecretSettings[key] {
			value, err = decryptWith(s.cipher, secretAAD("platform", "setting", key, "value"), value)
			if err != nil {
				return PlatformAuthSettings{}, err
			}
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return PlatformAuthSettings{}, err
	}
	return PlatformAuthSettings{
		GoogleEnabled:  values[PlatformSettingGoogleEnabled] == "1" || strings.EqualFold(values[PlatformSettingGoogleEnabled], "true"),
		GoogleClientID: values[PlatformSettingGoogleClientID], GoogleClientSecret: values[PlatformSettingGoogleClientSecret],
		TurnstileEnabled: values[PlatformSettingTurnstileEnabled] == "1" || strings.EqualFold(values[PlatformSettingTurnstileEnabled], "true"),
		TurnstileSiteKey: values[PlatformSettingTurnstileSiteKey], TurnstileSecret: values[PlatformSettingTurnstileSecret],
	}, nil
}

// GetPlatformAnalyticsSettings reads the operator-configured analytics
// integration. Missing rows use disabled/empty defaults. These values are not
// secrets and are safe to serve to the browser.
func (s *Store) GetPlatformAnalyticsSettings(ctx context.Context) (PlatformAnalyticsSettings, error) {
	keys := []string{
		PlatformSettingAnalyticsEnabled, PlatformSettingAnalyticsProvider,
		PlatformSettingAnalyticsScriptURL, PlatformSettingAnalyticsSiteID,
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM platform_settings WHERE key IN (`+placeholders+`)`, args...)
	if err != nil {
		return PlatformAnalyticsSettings{}, err
	}
	defer rows.Close()
	values := make(map[string]string, len(keys))
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return PlatformAnalyticsSettings{}, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return PlatformAnalyticsSettings{}, err
	}
	enabled := values[PlatformSettingAnalyticsEnabled]
	return PlatformAnalyticsSettings{
		Enabled:   enabled == "1" || strings.EqualFold(enabled, "true"),
		Provider:  values[PlatformSettingAnalyticsProvider],
		ScriptURL: values[PlatformSettingAnalyticsScriptURL],
		SiteID:    values[PlatformSettingAnalyticsSiteID],
	}, nil
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// GetPlatformMailSettings reads the platform mail configuration. Public fields
// (provider, from address, from name, SMTP endpoint) are always returned, even
// when the secret cipher is locked: they are stored in plaintext and must never
// be blanked just because a secret could not be decrypted. Only the secret
// fields degrade, and in that case the returned error wraps ErrSecretsLocked so
// callers can report "secrets unavailable" while still rendering the rest.
func (s *Store) GetPlatformMailSettings(ctx context.Context) (PlatformMailSettings, error) {
	keys := []string{
		PlatformSettingMailProvider, PlatformSettingMailFrom, PlatformSettingMailFromName, PlatformSettingMailResendAPIKey,
		PlatformSettingMailBrevoAPIKey, PlatformSettingMailSMTPHost, PlatformSettingMailSMTPPort,
		PlatformSettingMailSMTPUsername, PlatformSettingMailSMTPPassword, PlatformSettingMailSMTPMode,
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
	var secretErr error
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return PlatformMailSettings{}, err
		}
		if platformSecretSettings[key] {
			value, err = decryptWith(s.cipher, secretAAD("platform", "setting", key, "value"), value)
			if err != nil {
				// Keep every public field; only the secret is unavailable.
				if !errors.Is(err, ErrSecretsLocked) {
					return PlatformMailSettings{}, err
				}
				secretErr = ErrSecretsLocked
				value = ""
			}
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return PlatformMailSettings{}, err
	}
	return PlatformMailSettings{
		Provider: values[PlatformSettingMailProvider], From: values[PlatformSettingMailFrom],
		FromName:     values[PlatformSettingMailFromName],
		ResendAPIKey: values[PlatformSettingMailResendAPIKey], BrevoAPIKey: values[PlatformSettingMailBrevoAPIKey],
		SMTPHost: values[PlatformSettingMailSMTPHost],
		SMTPPort: values[PlatformSettingMailSMTPPort], SMTPUsername: values[PlatformSettingMailSMTPUsername],
		SMTPPassword: values[PlatformSettingMailSMTPPassword], SMTPMode: values[PlatformSettingMailSMTPMode],
	}, secretErr
}

// PlatformSettingMailSeeded is the marker recording that the environment mail
// bootstrap has already run. Without it, "never configured" and "deliberately
// cleared in the dashboard" are indistinguishable: a cleared value looks
// absent, so the next boot would silently re-seed the environment value.
const PlatformSettingMailSeeded = "mail_env_seeded"

// SeedPlatformMailSettings writes only keys that are currently absent, and only
// once ever. It is used for the one-time environment bootstrap; after the first
// seed the dashboard/database values win, including a deliberate clear.
func (s *Store) SeedPlatformMailSettings(ctx context.Context, settings PlatformMailSettings) error {
	seeded, err := s.platformMailSeeded(ctx)
	if err != nil || seeded {
		return err
	}
	values := map[string]string{
		PlatformSettingMailProvider: settings.Provider, PlatformSettingMailFrom: settings.From,
		PlatformSettingMailFromName:     settings.FromName,
		PlatformSettingMailResendAPIKey: settings.ResendAPIKey, PlatformSettingMailBrevoAPIKey: settings.BrevoAPIKey,
		PlatformSettingMailSMTPHost: settings.SMTPHost,
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
	return s.SetPlatformSetting(ctx, PlatformSettingMailSeeded, "1")
}

// platformMailSeeded reports whether the one-time environment mail bootstrap
// has already run. Installations seeded before the marker existed are detected
// from a persisted provider row, so they are not re-seeded either.
func (s *Store) platformMailSeeded(ctx context.Context) (bool, error) {
	var marker int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM platform_settings WHERE key=?`, PlatformSettingMailSeeded).Scan(&marker)
	if err != nil {
		return false, err
	}
	if marker != 0 {
		return true, nil
	}
	// ONE-TIME UPGRADE GUARD: a mail row written before the marker existed
	// means the bootstrap already ran (or an operator configured mail in the
	// dashboard). Backfill the marker rather than re-seeding over it.
	var provider int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM platform_settings WHERE key=?`, PlatformSettingMailProvider).Scan(&provider); err != nil {
		return false, err
	}
	if provider == 0 {
		return false, nil
	}
	return true, s.SetPlatformSetting(ctx, PlatformSettingMailSeeded, "1")
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
