package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"

	"github.com/tiller-router/tiller-router/internal/crypto"
)

// SecretCipher encrypts and decrypts recoverable tenant secrets at the store
// boundary. *crypto.Cipher implements it; tests may inject a fake.
type SecretCipher interface {
	Enabled() bool
	Locked() bool
	Encrypt(aad []byte, plaintext string) (string, error)
	Decrypt(aad []byte, stored string) (string, error)
}

// ErrSecretsLocked is returned when a secret is read or written while the
// master key is missing or does not match the stored ciphertext. Callers must
// treat it as an explicit "credentials unavailable" condition, never as a
// generic 500.
var ErrSecretsLocked = errors.New("store: provider credentials are locked")

// SecretsState names a cipher's state for logs and the admin status API:
// "enabled", "locked", or "disabled" (no cipher configured).
func SecretsState(c SecretCipher) string {
	switch {
	case c == nil:
		return "disabled"
	case c.Locked():
		return "locked"
	case !c.Enabled():
		return "disabled"
	default:
		return "enabled"
	}
}

// disabledCipher is the default when no cipher is configured. It is a
// passthrough: it never encrypts. Production always injects a real cipher
// (cmd/tiller-router resolves one at startup); this exists so unit tests and
// hash-only paths do not need one. It is distinct from the locked state.
type disabledCipher struct{}

func (disabledCipher) Enabled() bool                                      { return false }
func (disabledCipher) Locked() bool                                       { return false }
func (disabledCipher) Encrypt(_ []byte, plaintext string) (string, error) { return plaintext, nil }
func (disabledCipher) Decrypt(_ []byte, stored string) (string, error)    { return stored, nil }

// secretSettingKeys names settings-table keys whose values are recoverable
// secrets. Non-listed settings stay plaintext.
func secretSettingKeys() []string {
	return []string{SettingNotificationsAuthHeader}
}

func platformSecretSettingKeys() []string {
	return []string{PlatformSettingMailResendAPIKey, PlatformSettingMailBrevoAPIKey, PlatformSettingMailSMTPPassword}
}

// secretAAD builds the associated data binding a secret to its account, record
// kind, record id, and field.
func secretAAD(accountID, kind, id, field string) []byte {
	return crypto.AAD("tiller", "secret-v1", accountID, kind, id, field)
}

// encryptSecret encrypts a non-empty secret for this scope.
func (s *Scope) encryptSecret(aad []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return encryptWith(s.cipher, aad, plaintext)
}

// decryptSecret decrypts a stored secret for this scope. Legacy plaintext is
// returned unchanged while the cipher is enabled.
func (s *Scope) decryptSecret(aad []byte, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	return decryptWith(s.cipher, aad, stored)
}

func encryptWith(c SecretCipher, aad []byte, plaintext string) (string, error) {
	if c == nil || c.Locked() {
		return "", ErrSecretsLocked
	}
	if !c.Enabled() {
		return plaintext, nil
	}
	return c.Encrypt(aad, plaintext)
}

func decryptWith(c SecretCipher, aad []byte, stored string) (string, error) {
	if c == nil || c.Locked() {
		return "", ErrSecretsLocked
	}
	if !c.Enabled() {
		return stored, nil
	}
	plaintext, err := c.Decrypt(aad, stored)
	if err != nil {
		if errors.Is(err, crypto.ErrLocked) || errors.Is(err, crypto.ErrMalformed) {
			return "", ErrSecretsLocked
		}
		return "", err
	}
	return plaintext, nil
}

// secretRecord is one non-empty recoverable secret column.
type secretRecord struct {
	accountID string
	kind      string
	id        string
	field     string
	value     string
}

// listSecrets loads every non-empty recoverable secret column in the tenant
// tables. All reads complete before any write, so no UPDATE runs while a query
// cursor is open on the same SQLite connection.
func listSecrets(ctx context.Context, db querier) ([]secretRecord, error) {
	var out []secretRecord

	rows, err := db.QueryContext(ctx, `SELECT account_id,id,credential_secret FROM providers WHERE credential_secret IS NOT NULL AND credential_secret<>''`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rec secretRecord
		if err := rows.Scan(&rec.accountID, &rec.id, &rec.value); err != nil {
			rows.Close()
			return nil, err
		}
		rec.kind, rec.field = "provider", "credential_secret"
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	oauthFields := []string{"access_token", "refresh_token", "id_token", "provider_data"}
	rows, err = db.QueryContext(ctx, `SELECT account_id,provider_id,coalesce(access_token,''),coalesce(refresh_token,''),coalesce(id_token,''),coalesce(provider_data,'') FROM provider_oauth_tokens`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var accountID, providerID string
		values := make([]string, len(oauthFields))
		if err := rows.Scan(&accountID, &providerID, &values[0], &values[1], &values[2], &values[3]); err != nil {
			rows.Close()
			return nil, err
		}
		for i, field := range oauthFields {
			if values[i] == "" {
				continue
			}
			out = append(out, secretRecord{accountID: accountID, kind: "oauth", id: providerID, field: field, value: values[i]})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	keys := secretSettingKeys()
	sort.Strings(keys)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err = db.QueryContext(ctx, `SELECT account_id,key,value FROM settings WHERE value<>'' AND key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rec secretRecord
		if err := rows.Scan(&rec.accountID, &rec.id, &rec.value); err != nil {
			rows.Close()
			return nil, err
		}
		rec.kind, rec.field = "setting", "value"
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	platformKeys := platformSecretSettingKeys()
	sort.Strings(platformKeys)
	placeholders = strings.TrimRight(strings.Repeat("?,", len(platformKeys)), ",")
	args = args[:0]
	for _, k := range platformKeys {
		args = append(args, k)
	}
	rows, err = db.QueryContext(ctx, `SELECT key,value FROM platform_settings WHERE value<>'' AND key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rec secretRecord
		if err := rows.Scan(&rec.id, &rec.value); err != nil {
			rows.Close()
			return nil, err
		}
		rec.accountID, rec.kind, rec.field = "platform", "setting", "value"
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return out, nil
}

// applySecret writes a new value back to the column a secretRecord came from.
func applySecret(ctx context.Context, db querier, rec secretRecord, newValue string) error {
	switch rec.kind {
	case "provider":
		_, err := db.ExecContext(ctx, `UPDATE providers SET credential_secret=? WHERE id=? AND account_id=?`, newValue, rec.id, rec.accountID)
		return err
	case "oauth":
		var query string
		switch rec.field {
		case "access_token":
			query = `UPDATE provider_oauth_tokens SET access_token=? WHERE provider_id=? AND account_id=?`
		case "refresh_token":
			query = `UPDATE provider_oauth_tokens SET refresh_token=? WHERE provider_id=? AND account_id=?`
		case "id_token":
			query = `UPDATE provider_oauth_tokens SET id_token=? WHERE provider_id=? AND account_id=?`
		case "provider_data":
			query = `UPDATE provider_oauth_tokens SET provider_data=? WHERE provider_id=? AND account_id=?`
		default:
			return errors.New("store: unknown oauth secret field")
		}
		_, err := db.ExecContext(ctx, query, newValue, rec.id, rec.accountID)
		return err
	case "setting":
		if rec.accountID == "platform" {
			_, err := db.ExecContext(ctx, `UPDATE platform_settings SET value=? WHERE key=?`, newValue, rec.id)
			return err
		}
		_, err := db.ExecContext(ctx, `UPDATE settings SET value=? WHERE account_id=? AND key=?`, newValue, rec.accountID, rec.id)
		return err
	default:
		return errors.New("store: unknown secret record kind")
	}
}

// walkSecrets applies mutate to every recoverable secret and writes back the
// values it returns. Reads complete before any write.
func walkSecrets(ctx context.Context, db querier, mutate func(secretRecord) (string, bool, error)) error {
	records, err := listSecrets(ctx, db)
	if err != nil {
		return err
	}
	for _, rec := range records {
		newValue, changed, err := mutate(rec)
		if err != nil {
			return err
		}
		if changed {
			if err := applySecret(ctx, db, rec, newValue); err != nil {
				return err
			}
		}
	}
	return nil
}

// HasEncryptedSecrets reports whether any recoverable secret column currently
// holds a v1 ciphertext. It is used at startup to decide whether a missing key
// may be auto-generated.
func HasEncryptedSecrets(ctx context.Context, db *sql.DB) (bool, error) {
	keys := secretSettingKeys()
	sort.Strings(keys)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, k)
	}
	query := `
SELECT
 (SELECT count(*) FROM providers WHERE credential_secret LIKE 'enc:v1:%')
 + (SELECT count(*) FROM provider_oauth_tokens WHERE access_token LIKE 'enc:v1:%' OR refresh_token LIKE 'enc:v1:%' OR id_token LIKE 'enc:v1:%' OR provider_data LIKE 'enc:v1:%')
 + (SELECT count(*) FROM settings WHERE value LIKE 'enc:v1:%' AND key IN (` + placeholders + `))
 + (SELECT count(*) FROM platform_settings WHERE value LIKE 'enc:v1:%' AND key IN (` + strings.TrimRight(strings.Repeat("?,", len(platformSecretSettingKeys())), ",") + `))`
	for _, key := range platformSecretSettingKeys() {
		args = append(args, key)
	}
	var n int
	if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// MigrateSecrets encrypts any remaining plaintext secrets in place and
// verifies that existing ciphertext decrypts with cipher. It returns the number
// of plaintext values migrated, and locked=true when existing ciphertext cannot
// be decrypted. The caller must then start in the locked state instead of
// serving credentials.
func MigrateSecrets(ctx context.Context, db *sql.DB, cipher SecretCipher) (migrated int, locked bool, err error) {
	if cipher == nil || cipher.Locked() {
		return 0, true, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	err = walkSecrets(ctx, tx, func(rec secretRecord) (string, bool, error) {
		aad := secretAAD(rec.accountID, rec.kind, rec.id, rec.field)
		if crypto.Encrypted(rec.value) {
			if _, derr := decryptWith(cipher, aad, rec.value); derr != nil {
				return "", false, derr
			}
			return "", false, nil
		}
		enc, eerr := encryptWith(cipher, aad, rec.value)
		if eerr != nil {
			return "", false, eerr
		}
		migrated++
		return enc, true, nil
	})
	if errors.Is(err, ErrSecretsLocked) {
		// Roll back any partial migration: a wrong key must change nothing.
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return migrated, false, nil
}

// RotateSecrets re-encrypts every recoverable secret from the old key set to
// the new one. Any value the old cipher cannot decrypt aborts the rotation
// (leaving the database untouched up to that point) so a wrong old key cannot
// silently corrupt credentials.
func RotateSecrets(ctx context.Context, db *sql.DB, oldCipher, newCipher SecretCipher) (rotated int, err error) {
	if newCipher == nil || newCipher.Locked() {
		return 0, ErrSecretsLocked
	}
	if oldCipher == nil || oldCipher.Locked() {
		return 0, ErrSecretsLocked
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	err = walkSecrets(ctx, tx, func(rec secretRecord) (string, bool, error) {
		aad := secretAAD(rec.accountID, rec.kind, rec.id, rec.field)
		plaintext, derr := decryptWith(oldCipher, aad, rec.value)
		if derr != nil {
			return "", false, derr
		}
		enc, eerr := encryptWith(newCipher, aad, plaintext)
		if eerr != nil {
			return "", false, eerr
		}
		rotated++
		return enc, true, nil
	})
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return rotated, nil
}
