package oauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrNoToken = errors.New("oauth token not found")

type AuthState string

const (
	AuthConnected         AuthState = "connected"
	AuthReconnectRequired AuthState = "reconnect_required"
	AuthUnavailable       AuthState = "unavailable"
)

type TokenResponse struct {
	AccessToken      string
	RefreshToken     string
	TokenType        string
	ExpiresIn        int64
	RefreshExpiresIn int64
	IDToken          string
	Scope            string
	AccountEmail     string
	AccountPlan      string
	ProviderData     map[string]any
}

type TokenRecord struct {
	ProviderID       string
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
	AuthState        AuthState
	LastRefreshAt    *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func MergeToken(old TokenRecord, response TokenResponse, now time.Time) (TokenRecord, error) {
	if response.AccessToken == "" {
		return TokenRecord{}, errors.New("oauth token response did not contain an access token")
	}
	record := old
	record.AccessToken = response.AccessToken
	if response.RefreshToken != "" {
		record.RefreshToken = response.RefreshToken
	}
	if response.TokenType != "" {
		record.TokenType = response.TokenType
	}
	if response.ExpiresIn > 0 {
		expires := now.UTC().Add(time.Duration(response.ExpiresIn) * time.Second)
		record.ExpiresAt = &expires
	}
	if response.RefreshExpiresIn > 0 {
		expires := now.UTC().Add(time.Duration(response.RefreshExpiresIn) * time.Second)
		record.RefreshExpiresAt = &expires
	}
	if response.IDToken != "" {
		record.IDToken = response.IDToken
	}
	if response.Scope != "" {
		record.Scope = response.Scope
	}
	if response.AccountEmail != "" {
		record.AccountEmail = response.AccountEmail
	}
	if response.AccountPlan != "" {
		record.AccountPlan = response.AccountPlan
	}
	if response.ProviderData != nil {
		record.ProviderData = response.ProviderData
	}
	record.AuthState = AuthConnected
	record.LastRefreshAt = timePtr(now)
	record.UpdatedAt = now.UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now.UTC()
	}
	if record.TokenType == "" {
		record.TokenType = "Bearer"
	}
	return record, nil
}

func RefreshNeeded(record TokenRecord, now time.Time, lead time.Duration) bool {
	if record.ExpiresAt == nil {
		return false
	}
	return !now.UTC().Add(lead).Before(*record.ExpiresAt)
}

func Classify(record TokenRecord, now time.Time) AuthState {
	if record.AuthState == AuthReconnectRequired || record.AuthState == AuthUnavailable {
		return record.AuthState
	}
	if record.AccessToken == "" {
		return AuthUnavailable
	}
	if record.ExpiresAt != nil && !now.UTC().Before(*record.ExpiresAt) && record.RefreshToken == "" {
		return AuthReconnectRequired
	}
	return AuthConnected
}

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

func (s *Store) Get(ctx context.Context, providerID string) (TokenRecord, error) {
	var r TokenRecord
	var expiresAt, refreshExpiresAt, lastRefreshAt, createdAt, updatedAt sql.NullString
	var authState string
	var providerData sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT provider_id,access_token,coalesce(refresh_token,''),token_type,expires_at,refresh_expires_at,coalesce(id_token,''),coalesce(scope,''),coalesce(account_email,''),coalesce(account_plan,''),auth_state,last_refresh_at,created_at,updated_at,provider_data FROM provider_oauth_tokens WHERE provider_id=?`, providerID).
		Scan(&r.ProviderID, &r.AccessToken, &r.RefreshToken, &r.TokenType, &expiresAt, &refreshExpiresAt, &r.IDToken, &r.Scope, &r.AccountEmail, &r.AccountPlan, &authState, &lastRefreshAt, &createdAt, &updatedAt, &providerData)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenRecord{}, ErrNoToken
	}
	if err != nil {
		return TokenRecord{}, err
	}
	r.AuthState = AuthState(authState)
	if providerData.Valid && providerData.String != "" {
		if err := json.Unmarshal([]byte(providerData.String), &r.ProviderData); err != nil {
			return TokenRecord{}, errors.New("invalid OAuth provider metadata")
		}
	}
	var parseErr error
	if r.ExpiresAt, parseErr = parseTime(expiresAt); parseErr != nil {
		return TokenRecord{}, parseErr
	}
	if r.RefreshExpiresAt, parseErr = parseTime(refreshExpiresAt); parseErr != nil {
		return TokenRecord{}, parseErr
	}
	if r.LastRefreshAt, parseErr = parseTime(lastRefreshAt); parseErr != nil {
		return TokenRecord{}, parseErr
	}
	if created, parseErr := parseTime(createdAt); parseErr != nil {
		return TokenRecord{}, parseErr
	} else if created != nil {
		r.CreatedAt = *created
	}
	if updated, parseErr := parseTime(updatedAt); parseErr != nil {
		return TokenRecord{}, parseErr
	} else if updated != nil {
		r.UpdatedAt = *updated
	}
	return r, nil
}

func (s *Store) Put(ctx context.Context, r TokenRecord) error {
	if r.TokenType == "" {
		r.TokenType = "Bearer"
	}
	if r.AuthState == "" {
		r.AuthState = AuthConnected
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO provider_oauth_tokens(provider_id,access_token,refresh_token,token_type,expires_at,refresh_expires_at,id_token,scope,account_email,account_plan,auth_state,last_refresh_at,created_at,updated_at,provider_data) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(provider_id) DO UPDATE SET access_token=excluded.access_token,refresh_token=excluded.refresh_token,token_type=excluded.token_type,expires_at=excluded.expires_at,refresh_expires_at=excluded.refresh_expires_at,id_token=excluded.id_token,scope=excluded.scope,account_email=excluded.account_email,account_plan=excluded.account_plan,auth_state=excluded.auth_state,last_refresh_at=excluded.last_refresh_at,updated_at=excluded.updated_at,provider_data=excluded.provider_data`, r.ProviderID, r.AccessToken, nullableString(r.RefreshToken), r.TokenType, nullableTime(r.ExpiresAt), nullableTime(r.RefreshExpiresAt), nullableString(r.IDToken), nullableString(r.Scope), nullableString(r.AccountEmail), nullableString(r.AccountPlan), r.AuthState, nullableTime(r.LastRefreshAt), r.CreatedAt.UTC().Format(time.RFC3339Nano), r.UpdatedAt.UTC().Format(time.RFC3339Nano), nullableJSON(r.ProviderData))
	return err
}

func (s *Store) Delete(ctx context.Context, providerID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM provider_oauth_tokens WHERE provider_id=?`, providerID)
	return err
}

func (s *Store) SetState(ctx context.Context, providerID string, state AuthState, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE provider_oauth_tokens SET auth_state=?,updated_at=? WHERE provider_id=?`, state, now.UTC().Format(time.RFC3339Nano), providerID)
	return err
}

func timePtr(v time.Time) *time.Time { v = v.UTC(); return &v }

func parseTime(v sql.NullString) (*time.Time, error) {
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func nullableTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.UTC().Format(time.RFC3339Nano)
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableJSON(v map[string]any) any {
	if len(v) == 0 {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return string(b)
}
