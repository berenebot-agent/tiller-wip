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
	"github.com/tiller-router/tiller-router/internal/store"
)

func newTestStore(t *testing.T, db *database.DB) *store.Scope {
	t.Helper()
	return store.New(db.SQL).For(database.LocalAccountID)
}

func testToken(t *testing.T, st *store.Scope, id string) TokenRecord {
	t.Helper()
	row, err := st.GetOAuthToken(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return TokenFromStore(row)
}

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
	flow, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback"); !errors.Is(err, ErrFlowActive) {
		t.Fatalf("second Begin error = %v, want ErrFlowActive", err)
	}
	if _, err := store.Consume("acct-1", "provider-1", "wrong-state"); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("wrong state error = %v, want ErrFlowInvalid", err)
	}
	if _, err := store.Consume("acct-1", "provider-1", flow.PKCE.State); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("reused flow error = %v, want ErrFlowInvalid", err)
	}
	flow, err = store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(flowLifetime)
	if _, err := store.Consume("acct-1", "provider-1", flow.PKCE.State); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expired flow error = %v, want ErrFlowExpired", err)
	}
}

func TestFlowStorePreservesRedirectURI(t *testing.T) {
	store := NewFlowStore(nil)
	flow, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := store.Consume("acct-1", "provider-1", flow.PKCE.State)
	if err != nil {
		t.Fatal(err)
	}
	if consumed.RedirectURI != flow.RedirectURI {
		t.Fatalf("redirect URI = %q, want %q", consumed.RedirectURI, flow.RedirectURI)
	}
}

func TestFlowStoreTakeByStateSingleUseAndBinding(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := NewFlowStore(func() time.Time { return now })
	flow, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	taken, err := store.TakeByState(flow.PKCE.State)
	if err != nil {
		t.Fatal(err)
	}
	if taken.AccountID != "acct-1" || taken.ProviderID != "provider-1" || taken.PKCE.Verifier != flow.PKCE.Verifier || taken.RedirectURI != flow.RedirectURI {
		t.Fatalf("taken = %+v, want account/provider/verifier/redirect from flow", taken)
	}
	// Single use: the state cannot be redeemed twice.
	if _, err := store.TakeByState(flow.PKCE.State); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("reused state error = %v, want ErrFlowInvalid", err)
	}
	// The provider entry is gone too, so a fresh Begin succeeds.
	if _, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback"); err != nil {
		t.Fatalf("Begin after TakeByState error = %v, want success", err)
	}
	if _, err := store.TakeByState(""); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("empty state error = %v, want ErrFlowInvalid", err)
	}
}

func TestFlowStoreTakeByStateExpiryAndCancel(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := NewFlowStore(func() time.Time { return now })
	expired, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(flowLifetime)
	if _, err := store.TakeByState(expired.PKCE.State); !errors.Is(err, ErrFlowExpired) {
		t.Fatalf("expired state error = %v, want ErrFlowExpired", err)
	}
	// Cancel must clear the state index as well as the provider entry.
	flow, err := store.Begin("acct-2", "provider-2", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	store.Cancel("acct-2", "provider-2")
	if _, err := store.TakeByState(flow.PKCE.State); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("cancelled state error = %v, want ErrFlowInvalid", err)
	}
}

func TestFlowStoreReplacementClearsSupersededState(t *testing.T) {
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	store := NewFlowStore(func() time.Time { return now })
	first, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(flowLifetime)
	second, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TakeByState(first.PKCE.State); !errors.Is(err, ErrFlowInvalid) {
		t.Fatalf("superseded state error = %v, want ErrFlowInvalid", err)
	}
	if _, err := store.TakeByState(second.PKCE.State); err != nil {
		t.Fatalf("current state error = %v, want success", err)
	}
}

func TestPersistentFlowStoreRebuildsStateIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flows.json")
	store := NewPersistentFlowStore(nil, path)
	flow, err := store.Begin("acct-1", "provider-1", "https://tiller.example.com/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	reloaded := NewPersistentFlowStore(nil, path)
	taken, err := reloaded.TakeByState(flow.PKCE.State)
	if err != nil {
		t.Fatal(err)
	}
	if taken.AccountID != "acct-1" || taken.ProviderID != "provider-1" {
		t.Fatalf("reloaded flow = %+v", taken)
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
	st := newTestStore(t, db)
	expired := time.Now().Add(-time.Minute)
	if err := st.PutOAuthToken(context.Background(), TokenToStore(TokenRecord{ProviderID: "provider-1", AccessToken: "old", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: &expired, AuthState: AuthConnected, CreatedAt: time.Now(), UpdatedAt: time.Now()})); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store.New(db.SQL), time.Minute)

	refreshReconnect := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, ErrReconnectRequired
	}
	_, err = manager.ForceRefresh(context.Background(), database.LocalAccountID, "provider-1", refreshReconnect)
	if !errors.Is(err, ErrReconnectRequired) {
		t.Fatalf("ForceRefresh error = %v, want ErrReconnectRequired", err)
	}
	record := testToken(t, st, "provider-1")
	if record.AuthState != AuthReconnectRequired {
		t.Fatalf("auth_state = %q, want reconnect_required", record.AuthState)
	}

	refreshUnavailable := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, ErrAuthUnavailable
	}
	_, err = manager.ForceRefresh(context.Background(), database.LocalAccountID, "provider-1", refreshUnavailable)
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("ForceRefresh error = %v, want ErrAuthUnavailable", err)
	}
	record = testToken(t, st, "provider-1")
	if record.AuthState != AuthUnavailable {
		t.Fatalf("auth_state = %q, want unavailable", record.AuthState)
	}

	refreshTransient := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, errors.New("network blip")
	}
	_, err = manager.ForceRefresh(context.Background(), database.LocalAccountID, "provider-1", refreshTransient)
	if err == nil {
		t.Fatal("ForceRefresh expected error, got nil")
	}
	record = testToken(t, st, "provider-1")
	if record.AuthState != AuthUnavailable {
		t.Fatalf("auth_state = %q, want unavailable (transient)", record.AuthState)
	}

	refreshOK := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{AccessToken: "new", RefreshToken: "rotated", ExpiresIn: 3600}, nil
	}
	_, err = manager.ForceRefresh(context.Background(), database.LocalAccountID, "provider-1", refreshOK)
	if err != nil {
		t.Fatalf("ForceRefresh success error = %v", err)
	}
	record = testToken(t, st, "provider-1")
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
	st := newTestStore(t, db)
	expired := time.Now().Add(-time.Minute)
	if err := st.PutOAuthToken(context.Background(), TokenToStore(TokenRecord{ProviderID: "provider-1", AccessToken: "old", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: &expired, AuthState: AuthConnected, CreatedAt: time.Now(), UpdatedAt: time.Now()})); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store.New(db.SQL), time.Minute)
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
			record, err := manager.Current(context.Background(), database.LocalAccountID, "provider-1", refresh)
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
	record := testToken(t, st, "provider-1")
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
	st := newTestStore(t, db)
	expired := time.Now().Add(-time.Minute)
	if err := st.PutOAuthToken(context.Background(), TokenToStore(TokenRecord{ProviderID: "provider-1", AccessToken: "old", RefreshToken: "refresh", TokenType: "Bearer", ExpiresAt: &expired, AuthState: AuthConnected, CreatedAt: time.Now(), UpdatedAt: time.Now()})); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store.New(db.SQL), time.Minute)

	// Refresh that fails with context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	refreshCanceled := func(context.Context, TokenRecord) (TokenResponse, error) {
		return TokenResponse{}, ctx.Err()
	}
	_, err = manager.ForceRefresh(ctx, database.LocalAccountID, "provider-1", refreshCanceled)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	record := testToken(t, st, "provider-1")
	// Auth state must remain connected — cancellation is transient.
	if record.AuthState != AuthConnected {
		t.Fatalf("auth_state = %q after cancellation, want connected", record.AuthState)
	}
}

func TestOAuthWritersRemainDisconnectedAcrossRaces(t *testing.T) {
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
	st := newTestStore(t, db)
	expired := time.Now().Add(-time.Minute)
	if err := st.PutOAuthToken(context.Background(), TokenToStore(TokenRecord{ProviderID: "provider-1", AccessToken: "old", RefreshToken: "refresh", ExpiresAt: &expired, AuthState: AuthConnected})); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store.New(db.SQL), time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})
	refreshDone := make(chan error, 1)
	go func() {
		_, refreshErr := manager.ForceRefresh(context.Background(), database.LocalAccountID, "provider-1", func(context.Context, TokenRecord) (TokenResponse, error) {
			close(started)
			<-release
			return TokenResponse{AccessToken: "late", RefreshToken: "late-refresh", ExpiresIn: 3600}, nil
		})
		refreshDone <- refreshErr
	}()
	<-started
	generation, err := st.AdvanceOAuthGeneration(context.Background(), "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteOAuthToken(context.Background(), "provider-1"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-refreshDone; !errors.Is(err, store.ErrOAuthGenerationChanged) {
		t.Fatalf("refresh error = %v, want generation changed", err)
	}
	if _, err := st.GetOAuthToken(context.Background(), "provider-1"); !errors.Is(err, store.ErrNoOAuthToken) {
		t.Fatalf("late refresh token error = %v, want no token", err)
	}
	late := TokenToStore(TokenRecord{ProviderID: "provider-1", AccessToken: "callback", RefreshToken: "callback-refresh", AuthState: AuthConnected})
	if err := st.PutOAuthTokenIfGeneration(context.Background(), late, generation-1); !errors.Is(err, store.ErrOAuthGenerationChanged) {
		t.Fatalf("callback write error = %v, want generation changed", err)
	}
	if err := st.PutOAuthTokenIfGeneration(context.Background(), late, generation-1); !errors.Is(err, store.ErrOAuthGenerationChanged) {
		t.Fatalf("device write error = %v, want generation changed", err)
	}
	if err := st.PutOAuthTokenIfGeneration(context.Background(), late, generation); err != nil {
		t.Fatalf("new connection error = %v", err)
	}
	if got := testToken(t, st, "provider-1"); got.AccessToken != "callback" {
		t.Fatalf("new connection access token = %q, want callback", got.AccessToken)
	}
}
