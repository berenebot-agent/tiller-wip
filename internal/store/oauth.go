package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrNoOAuthToken is returned when an account has no stored OAuth token for a
// provider.
var ErrNoOAuthToken = errors.New("store: no oauth token")

// OAuthTokenRow is the persisted OAuth token state for one provider.
type OAuthTokenRow struct {
	ProviderID       string
	Generation       int64
	AccessToken      string
	RefreshToken     string
	TokenType        string
	ExpiresAt        *time.Time
	RefreshExpiresAt *time.Time
	IDToken          string
	Scope            string
	AccountEmail     string
	AccountPlan      string
	ProviderData     map[string]any
	AuthState        string
	LastRefreshAt    *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (s *Scope) GetOAuthToken(ctx context.Context, providerID string) (OAuthTokenRow, error) {
	var r OAuthTokenRow
	var expiresAt, refreshExpiresAt, lastRefreshAt, createdAt, updatedAt sql.NullString
	var providerData sql.NullString
	err := s.q.QueryRowContext(ctx, `SELECT provider_id,generation,access_token,coalesce(refresh_token,''),token_type,expires_at,refresh_expires_at,coalesce(id_token,''),coalesce(scope,''),coalesce(account_email,''),coalesce(account_plan,''),auth_state,last_refresh_at,created_at,updated_at,provider_data FROM provider_oauth_tokens WHERE provider_id=? AND account_id=?`, providerID, s.accountID).
		Scan(&r.ProviderID, &r.Generation, &r.AccessToken, &r.RefreshToken, &r.TokenType, &expiresAt, &refreshExpiresAt, &r.IDToken, &r.Scope, &r.AccountEmail, &r.AccountPlan, &r.AuthState, &lastRefreshAt, &createdAt, &updatedAt, &providerData)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthTokenRow{}, ErrNoOAuthToken
	}
	if err != nil {
		return OAuthTokenRow{}, err
	}
	var decErr error
	if r.AccessToken, decErr = s.decryptSecret(secretAAD(s.accountID, "oauth", providerID, "access_token"), r.AccessToken); decErr != nil {
		return OAuthTokenRow{}, decErr
	}
	if r.RefreshToken, decErr = s.decryptSecret(secretAAD(s.accountID, "oauth", providerID, "refresh_token"), r.RefreshToken); decErr != nil {
		return OAuthTokenRow{}, decErr
	}
	if r.IDToken, decErr = s.decryptSecret(secretAAD(s.accountID, "oauth", providerID, "id_token"), r.IDToken); decErr != nil {
		return OAuthTokenRow{}, decErr
	}
	if providerData.Valid && providerData.String != "" {
		decrypted, derr := s.decryptSecret(secretAAD(s.accountID, "oauth", providerID, "provider_data"), providerData.String)
		if derr != nil {
			return OAuthTokenRow{}, derr
		}
		providerData.String = decrypted
		if err := json.Unmarshal([]byte(providerData.String), &r.ProviderData); err != nil {
			return OAuthTokenRow{}, errors.New("invalid OAuth provider metadata")
		}
	}
	var parseErr error
	if r.ExpiresAt, parseErr = parseStoreTime(expiresAt); parseErr != nil {
		return OAuthTokenRow{}, parseErr
	}
	if r.RefreshExpiresAt, parseErr = parseStoreTime(refreshExpiresAt); parseErr != nil {
		return OAuthTokenRow{}, parseErr
	}
	if r.LastRefreshAt, parseErr = parseStoreTime(lastRefreshAt); parseErr != nil {
		return OAuthTokenRow{}, parseErr
	}
	if created, parseErr := parseStoreTime(createdAt); parseErr != nil {
		return OAuthTokenRow{}, parseErr
	} else if created != nil {
		r.CreatedAt = *created
	}
	if updated, parseErr := parseStoreTime(updatedAt); parseErr != nil {
		return OAuthTokenRow{}, parseErr
	} else if updated != nil {
		r.UpdatedAt = *updated
	}
	return r, nil
}

var ErrOAuthGenerationChanged = errors.New("store: oauth connection generation changed")

func (s *Scope) OAuthGeneration(ctx context.Context, providerID string) (int64, error) {
	var generation int64
	err := s.q.QueryRowContext(ctx, `SELECT generation FROM oauth_connection_generations WHERE account_id=? AND provider_id=?`, s.accountID, providerID).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return generation, err
}

func (s *Scope) PutOAuthTokenIfGeneration(ctx context.Context, r OAuthTokenRow, generation int64) error {
	if r.TokenType == "" {
		r.TokenType = "Bearer"
	}
	if r.AuthState == "" {
		r.AuthState = "connected"
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	accessToken, err := s.encryptSecret(secretAAD(s.accountID, "oauth", r.ProviderID, "access_token"), r.AccessToken)
	if err != nil {
		return err
	}
	refreshToken, err := s.encryptSecret(secretAAD(s.accountID, "oauth", r.ProviderID, "refresh_token"), r.RefreshToken)
	if err != nil {
		return err
	}
	idToken, err := s.encryptSecret(secretAAD(s.accountID, "oauth", r.ProviderID, "id_token"), r.IDToken)
	if err != nil {
		return err
	}
	var providerData any
	if pd := nullableStoreJSON(r.ProviderData); pd != nil {
		enc, eerr := s.encryptSecret(secretAAD(s.accountID, "oauth", r.ProviderID, "provider_data"), pd.(string))
		if eerr != nil {
			return eerr
		}
		providerData = enc
	}
	result, err := s.q.ExecContext(ctx, `INSERT INTO provider_oauth_tokens(account_id,provider_id,generation,access_token,refresh_token,token_type,expires_at,refresh_expires_at,id_token,scope,account_email,account_plan,auth_state,last_refresh_at,created_at,updated_at,provider_data) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,? WHERE COALESCE((SELECT generation FROM oauth_connection_generations WHERE account_id=? AND provider_id=?),0)=? ON CONFLICT(provider_id) DO UPDATE SET generation=excluded.generation,access_token=excluded.access_token,refresh_token=excluded.refresh_token,token_type=excluded.token_type,expires_at=excluded.expires_at,refresh_expires_at=excluded.refresh_expires_at,id_token=excluded.id_token,scope=excluded.scope,account_email=excluded.account_email,account_plan=excluded.account_plan,auth_state=excluded.auth_state,last_refresh_at=excluded.last_refresh_at,updated_at=excluded.updated_at,provider_data=excluded.provider_data WHERE provider_oauth_tokens.account_id=? AND provider_oauth_tokens.generation=?`, s.accountID, r.ProviderID, generation, accessToken, nullableStoreString(refreshToken), r.TokenType, nullableStoreTime(r.ExpiresAt), nullableStoreTime(r.RefreshExpiresAt), nullableStoreString(idToken), nullableStoreString(r.Scope), nullableStoreString(r.AccountEmail), nullableStoreString(r.AccountPlan), r.AuthState, nullableStoreTime(r.LastRefreshAt), r.CreatedAt.UTC().Format(time.RFC3339Nano), r.UpdatedAt.UTC().Format(time.RFC3339Nano), providerData, s.accountID, r.ProviderID, generation, s.accountID, generation)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrOAuthGenerationChanged
	}
	return nil
}

func (s *Scope) PutOAuthToken(ctx context.Context, r OAuthTokenRow) error {
	generation, err := s.OAuthGeneration(ctx, r.ProviderID)
	if err != nil {
		return err
	}
	return s.PutOAuthTokenIfGeneration(ctx, r, generation)
}

func (s *Scope) AdvanceOAuthGeneration(ctx context.Context, providerID string) (int64, error) {
	_, err := s.q.ExecContext(ctx, `INSERT INTO oauth_connection_generations(account_id,provider_id,generation) VALUES(?,?,1) ON CONFLICT(account_id,provider_id) DO UPDATE SET generation=generation+1`, s.accountID, providerID)
	if err != nil {
		return 0, err
	}
	return s.OAuthGeneration(ctx, providerID)
}

func (s *Scope) DeleteOAuthToken(ctx context.Context, providerID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM provider_oauth_tokens WHERE provider_id=? AND account_id=?`, providerID, s.accountID)
	return err
}

func (s *Scope) SetOAuthState(ctx context.Context, providerID, state string, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE provider_oauth_tokens SET auth_state=?,updated_at=? WHERE provider_id=? AND account_id=?`, state, at.UTC().Format(time.RFC3339Nano), providerID, s.accountID)
	return err
}

func parseStoreTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullableStoreTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.UTC().Format(time.RFC3339Nano)
}

func nullableStoreString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableStoreJSON(v map[string]any) any {
	if len(v) == 0 {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}
