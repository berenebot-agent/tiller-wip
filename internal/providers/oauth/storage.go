package oauth

import (
	"errors"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
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
	if response.ExpiresIn != 0 {
		var expires time.Time
		if response.ExpiresIn < 0 {
			expires = now.UTC()
		} else {
			expires = now.UTC().Add(time.Duration(response.ExpiresIn) * time.Second)
		}
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

func timePtr(v time.Time) *time.Time { v = v.UTC(); return &v }

// TokenToStore converts a TokenRecord into its account-scoped persistence row.
func TokenToStore(r TokenRecord) store.OAuthTokenRow {
	return store.OAuthTokenRow{
		ProviderID:       r.ProviderID,
		Generation:       r.Generation,
		AccessToken:      r.AccessToken,
		RefreshToken:     r.RefreshToken,
		TokenType:        r.TokenType,
		ExpiresAt:        r.ExpiresAt,
		RefreshExpiresAt: r.RefreshExpiresAt,
		IDToken:          r.IDToken,
		Scope:            r.Scope,
		AccountEmail:     r.AccountEmail,
		AccountPlan:      r.AccountPlan,
		ProviderData:     r.ProviderData,
		AuthState:        string(r.AuthState),
		LastRefreshAt:    r.LastRefreshAt,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

// TokenFromStore converts a persistence row back into a TokenRecord.
func TokenFromStore(row store.OAuthTokenRow) TokenRecord {
	return TokenRecord{
		ProviderID:       row.ProviderID,
		Generation:       row.Generation,
		AccessToken:      row.AccessToken,
		RefreshToken:     row.RefreshToken,
		TokenType:        row.TokenType,
		ExpiresAt:        row.ExpiresAt,
		RefreshExpiresAt: row.RefreshExpiresAt,
		IDToken:          row.IDToken,
		Scope:            row.Scope,
		AccountEmail:     row.AccountEmail,
		AccountPlan:      row.AccountPlan,
		ProviderData:     row.ProviderData,
		AuthState:        AuthState(row.AuthState),
		LastRefreshAt:    row.LastRefreshAt,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}
