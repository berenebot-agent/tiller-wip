package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestNewPKCEAndParseCallback(t *testing.T) {
	pkce, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(pkce.Verifier))
	if got := base64.RawURLEncoding.EncodeToString(hash[:]); got != pkce.Challenge {
		t.Fatalf("challenge = %q, want %q", pkce.Challenge, got)
	}
	callbackURL := (&url.URL{Scheme: "https", Host: "callback.invalid", RawQuery: url.Values{"code": {"authorization-code"}, "state": {pkce.State}}.Encode()}).String()
	callback, err := ParseCallback(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	if callback.Code != "authorization-code" || callback.State != pkce.State {
		t.Fatalf("callback = %+v", callback)
	}
	for _, invalid := range []string{"authorization-code", "https://callback.invalid/?state=" + pkce.State, "https://callback.invalid/?code=authorization-code"} {
		if _, err := ParseCallback(invalid); !errors.Is(err, ErrCallback) {
			t.Errorf("ParseCallback(%q) error = %v, want ErrCallback", invalid, err)
		}
	}
}

func TestFlowStoreOneActiveAndSingleUse(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := NewFlowStore(func() time.Time { return now })
	flow, err := store.Begin("provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin("provider-1"); !errors.Is(err, ErrFlowActive) {
		t.Fatalf("second Begin error = %v, want ErrFlowActive", err)
	}
	if _, err := store.Consume("provider-1", "wrong-state"); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("wrong state error = %v, want ErrFlowInvalid", err)
	}
	if _, err := store.Consume("provider-1", flow.PKCE.State); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("reused flow error = %v, want ErrFlowInvalid", err)
	}
	flow, err = store.Begin("provider-1")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(flowLifetime)
	if _, err := store.Consume("provider-1", flow.PKCE.State); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expired flow error = %v, want ErrFlowExpired", err)
	}
}

func TestMergeTokenPreservesOmittedRefreshToken(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	old := TokenRecord{ProviderID: "provider-1", AccessToken: "old-access", RefreshToken: "durable-refresh", TokenType: "Bearer", AuthState: AuthReconnectRequired, CreatedAt: now.Add(-time.Hour)}
	merged, err := MergeToken(old, TokenResponse{AccessToken: "new-access", ExpiresIn: 3600, AccountEmail: "user@example.test"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if merged.AccessToken != "new-access" || merged.RefreshToken != "durable-refresh" || merged.AuthState != AuthConnected {
		t.Fatalf("merged token = %+v", merged)
	}
	if merged.ExpiresAt == nil || !merged.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("expires_at = %v", merged.ExpiresAt)
	}
}

func TestPollDeviceCode(t *testing.T) {
	var polls atomic.Int32
	token, err := PollDeviceCode(context.Background(), time.Now().Add(time.Second), time.Millisecond, func(context.Context) DevicePollResult {
		if polls.Add(1) == 1 {
			return DevicePollResult{Status: DevicePending}
		}
		return DevicePollResult{Status: DeviceSuccess, Token: TokenResponse{AccessToken: "access"}}
	})
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "access" || polls.Load() != 2 {
		t.Fatalf("token=%+v polls=%d", token, polls.Load())
	}
	if _, err := PollDeviceCode(context.Background(), time.Now().Add(time.Second), time.Millisecond, func(context.Context) DevicePollResult { return DevicePollResult{Status: DeviceDenied} }); !errors.Is(err, ErrDeviceDenied) {
		t.Fatalf("denied error = %v", err)
	}
}

func TestForceRefreshTransitionsAuthStateOnFailure(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('oauth-provider','real','provider-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('provider-1','oauth-provider','codex-subscription','https://provider.invalid',1,'["responses"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db.SQL)
	expired := time.Now().Add(-time.Minute)
	if err := store.Put(context.Background(), TokenRecord{ProviderID: "provider-1", AccessToken: "old", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: &expired, AuthState: AuthConnected, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, time.Minute)

	refreshReconnect := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, ErrReconnectRequired
	}
	_, err = manager.ForceRefresh(context.Background(), "provider-1", refreshReconnect)
	if !errors.Is(err, ErrReconnectRequired) {
		t.Fatalf("ForceRefresh error = %v, want ErrReconnectRequired", err)
	}
	record, err := store.Get(context.Background(), "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.AuthState != AuthReconnectRequired {
		t.Fatalf("auth_state = %q, want reconnect_required", record.AuthState)
	}

	refreshUnavailable := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, ErrAuthUnavailable
	}
	_, err = manager.ForceRefresh(context.Background(), "provider-1", refreshUnavailable)
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("ForceRefresh error = %v, want ErrAuthUnavailable", err)
	}
	record, err = store.Get(context.Background(), "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.AuthState != AuthUnavailable {
		t.Fatalf("auth_state = %q, want unavailable", record.AuthState)
	}

	refreshTransient := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, errors.New("network blip")
	}
	_, err = manager.ForceRefresh(context.Background(), "provider-1", refreshTransient)
	if err == nil {
		t.Fatal("ForceRefresh expected error, got nil")
	}
	record, err = store.Get(context.Background(), "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.AuthState != AuthUnavailable {
		t.Fatalf("auth_state = %q, want unavailable (transient)", record.AuthState)
	}

	refreshOK := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{AccessToken: "new", RefreshToken: "rotated", ExpiresIn: 3600}, nil
	}
	_, err = manager.ForceRefresh(context.Background(), "provider-1", refreshOK)
	if err != nil {
		t.Fatalf("ForceRefresh success error = %v", err)
	}
	record, err = store.Get(context.Background(), "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.AuthState != AuthConnected {
		t.Fatalf("auth_state = %q, want connected", record.AuthState)
	}
	if record.AccessToken != "new" {
		t.Fatalf("access_token = %q, want new", record.AccessToken)
	}
}

func TestRefreshManagerDeduplicatesConcurrentRefresh(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('oauth-provider','real','provider-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('provider-1','oauth-provider','codex-subscription','https://provider.invalid',1,'["responses"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db.SQL)
	expired := time.Now().Add(-time.Minute)
	if err := store.Put(context.Background(), TokenRecord{ProviderID: "provider-1", AccessToken: "old", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: &expired, AuthState: AuthConnected, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, time.Minute)
	var calls atomic.Int32
	refresh := func(context.Context, TokenRecord) (TokenResponse, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return TokenResponse{AccessToken: "new", RefreshToken: "rotated", ExpiresIn: 3600}, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			record, err := manager.Current(context.Background(), "provider-1", refresh)
			if err != nil {
				errs <- err
			} else if record.AccessToken != "new" {
				errs <- errors.New("caller received stale access token")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
	record, err := store.Get(context.Background(), "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.RefreshToken != "rotated" {
		t.Fatalf("stored refresh token = %q", record.RefreshToken)
	}
}

// TestForceRefreshTransientOnContextCancellation verifies that a context
// cancellation during refresh does NOT transition auth_state — the failure
// must stay retryable.
func TestForceRefreshTransientOnContextCancellation(t *testing.T) {
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := database.Now()
	if _, err := db.SQL.Exec(`INSERT INTO namespaces(name,kind,entity_id) VALUES('oauth-provider','real','provider-1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO providers(id,name,type,base_url,enabled,protocols,created_at,updated_at) VALUES('provider-1','oauth-provider','codex-subscription','https://provider.invalid',1,'["responses"]',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db.SQL)
	expired := time.Now().Add(-time.Minute)
	if err := store.Put(context.Background(), TokenRecord{ProviderID: "provider-1", AccessToken: "old", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: &expired, AuthState: AuthConnected, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store, time.Minute)

	// Refresh that fails with context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	refreshCanceled := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, ctx.Err()
	}
	_, err = manager.ForceRefresh(ctx, "provider-1", refreshCanceled)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	record, err := store.Get(context.Background(), "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	// Auth state must remain connected — cancellation is transient.
	if record.AuthState != AuthConnected {
		t.Fatalf("auth_state = %q after cancellation, want connected", record.AuthState)
	}
}
