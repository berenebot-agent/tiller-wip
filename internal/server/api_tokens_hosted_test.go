package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/store"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

const hostedTestPassword = "correct horse battery staple"

func hostedExpiry(days int) string {
	return time.Now().Add(time.Duration(days) * 24 * time.Hour).UTC().Format(time.RFC3339)
}

// hostedMintToken creates a token through the cookie session.
func hostedMintToken(t *testing.T, api *testAPI, name string, scopes map[string]int) (id, token string) {
	t.Helper()
	status, payload, _ := api.request("POST", "/api/admin/api-tokens", map[string]any{"name": name, "scopes": scopes, "expires_at": hostedExpiry(30)})
	if status != 201 {
		t.Fatalf("mint hosted token %q: %d %v", name, status, payload)
	}
	return payload["id"].(string), payload["token"].(string)
}

func TestHostedTokenCreatePolicy(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	good := map[string]any{"name": "ci", "scopes": map[string]int{"client_keys": 2}, "expires_at": hostedExpiry(30)}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range good {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	cases := []struct {
		name  string
		extra map[string]any
		code  string
	}{
		{"delete tier", map[string]any{"scopes": map[string]int{"providers": 3}}, "invalid_scopes"},
	}
	for _, tc := range cases {
		status, payload, _ := api.request("POST", "/api/admin/api-tokens", with(tc.extra))
		if status != 400 || payload["error"].(map[string]any)["code"] != tc.code {
			t.Fatalf("%s: %d %v, want 400 %s", tc.name, status, payload, tc.code)
		}
	}
	if status, payload, _ := api.request("POST", "/api/admin/api-tokens", with(nil)); status != 201 {
		t.Fatalf("valid create: %d %v", status, payload)
	}
	// A token without an expiry is allowed in hosted mode.
	status, payload, _ := api.request("POST", "/api/admin/api-tokens", with(map[string]any{"name": "forever", "expires_at": ""}))
	if status != 201 || payload["expires_at"] != nil {
		t.Fatalf("no-expiry create: %d %v, want 201 with null expiry", status, payload)
	}
}

func TestHostedTokenCountCap(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	for i := 0; i < hostedMaxManagementTokens; i++ {
		hostedMintToken(t, api, fmt.Sprintf("t%d", i), map[string]int{})
	}
	status, payload, _ := api.request("POST", "/api/admin/api-tokens", map[string]any{"name": "one-too-many", "scopes": map[string]int{}, "expires_at": hostedExpiry(1)})
	if status != 409 || payload["error"].(map[string]any)["code"] != "token_limit_reached" {
		t.Fatalf("over cap: %d %v", status, payload)
	}
}

func TestHostedTokenUseAndBoundaries(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	_, token := hostedMintToken(t, api, "reader", map[string]int{"client_keys": 1})

	if status, payload, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil); status != 200 {
		t.Fatalf("token read: %d %v", status, payload)
	}
	status, payload, _ := bearerRequest(t, api.base, token, "POST", "/api/admin/client-keys", map[string]any{"name": "nope"})
	if status != 403 || payload["error"].(map[string]any)["code"] != "insufficient_scope" {
		t.Fatalf("token write above ceiling: %d %v", status, payload)
	}
	if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/providers", nil); status != 403 {
		t.Fatalf("token outside granted area = %d, want 403", status)
	}
	if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/api-tokens/self", nil); status != 200 {
		t.Fatalf("self introspection = %d, want 200", status)
	}
	// Account, identity, export, lifecycle and audit surfaces are cookie-only.
	for _, route := range [][2]string{
		{"GET", "/api/admin/api-tokens"},
		{"POST", "/api/admin/api-tokens"},
		{"POST", "/api/admin/api-tokens/revoke-all"},
		{"GET", "/api/auth/account"},
		{"GET", "/api/auth/account/export"},
		{"DELETE", "/api/auth/account"},
		{"POST", "/api/auth/account/sessions/revoke-all"},
		{"POST", "/api/auth/account/password"},
		{"GET", "/api/auth/account/passkeys"},
		{"GET", "/api/admin/audit"},
	} {
		var body map[string]any
		if route[0] != "GET" {
			body = map[string]any{}
		}
		if status, _, _ := bearerRequest(t, api.base, token, route[0], route[1], body); status != 401 {
			t.Fatalf("token on %s %s = %d, want 401", route[0], route[1], status)
		}
	}
}

func TestHostedTokenEditAndRotateNeedNoPassword(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	id, token := hostedMintToken(t, api, "grow", map[string]int{"client_keys": 1})

	if status, payload, _ := api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"scopes": map[string]int{"client_keys": 2}}); status != 200 {
		t.Fatalf("widen: %d %v", status, payload)
	}
	if status, payload, _ := api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"scopes": map[string]int{"client_keys": 1}, "enabled": false}); status != 200 {
		t.Fatalf("narrow/disable: %d %v", status, payload)
	}
	if status, payload, _ := api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"enabled": true}); status != 200 {
		t.Fatalf("re-enable: %d %v", status, payload)
	}
	if status, payload, _ := api.request("PATCH", "/api/admin/api-tokens/"+id, map[string]any{"expires_at": ""}); status != 200 || payload["expires_at"] != nil {
		t.Fatalf("clear expiry: %d %v", status, payload)
	}

	// Rotation kills the old secret immediately.
	status, payload, _ := api.request("POST", "/api/admin/api-tokens/"+id+"/rotate", nil)
	if status != 200 {
		t.Fatalf("rotate: %d %v", status, payload)
	}
	if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil); status != 401 {
		t.Fatalf("old secret after rotate = %d, want 401", status)
	}
	if status, _, _ := bearerRequest(t, api.base, payload["token"].(string), "GET", "/api/admin/client-keys", nil); status != 200 {
		t.Fatalf("new secret after rotate = %d, want 200", status)
	}
}

func TestHostedTokenCrossAccountIsolation(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	_, tokenA := hostedMintToken(t, api, "a", map[string]int{"client_keys": 2, "providers": 2})

	// Account B with its own token, seeded directly through the store.
	const accountB = "acct-b-hosted-token-test"
	if _, err := app.db.SQL.Exec(`INSERT INTO accounts(id,plan,status,created_at,updated_at) VALUES(?,'free','active','2026-11-09T00:00:00Z','2026-11-09T00:00:00Z')`, accountB); err != nil {
		t.Fatal(err)
	}
	generated, err := auth.GenerateManagementTokenWithHasher(fastsecret.Hasher{})
	if err != nil {
		t.Fatal(err)
	}
	scopes, _ := auth.ValidateManagementScopes(map[string]int{"client_keys": 2})
	if err := app.scopeFor(accountB).CreateManagementToken(context.Background(), storeCreateToken("b-token", "b", generated, scopes)); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := bearerRequest(t, api.base, generated.Plaintext, "POST", "/api/admin/client-keys", map[string]any{"name": "key-b"})
	if status != 201 {
		t.Fatalf("B creates key: %d %v", status, payload)
	}
	keyB := payload["id"].(string)

	// A's token cannot see or touch B's key.
	status, payload, _ = bearerRequest(t, api.base, tokenA, "GET", "/api/admin/client-keys", nil)
	if status != 200 || listContains(payload, "id", keyB) {
		t.Fatalf("A sees B's key: %d %v", status, payload)
	}
	if status, _, _ := bearerRequest(t, api.base, tokenA, "PATCH", "/api/admin/client-keys/"+keyB, map[string]any{"name": "pwned"}); status != 404 {
		t.Fatalf("A patches B's key = %d, want 404", status)
	}
	if status, _, _ := bearerRequest(t, api.base, tokenA, "POST", "/api/admin/client-keys/"+keyB+"/rotate", nil); status != 404 {
		t.Fatalf("A rotates B's key = %d, want 404", status)
	}
	// A's session also cannot see B's token.
	status, payload, _ = api.request("GET", "/api/admin/api-tokens", nil)
	if status != 200 || listContains(payload, "id", "b-token") {
		t.Fatalf("A lists B's token: %d %v", status, payload)
	}
	if status, _, _ := api.request("DELETE", "/api/admin/api-tokens/b-token", nil); status != 404 {
		t.Fatalf("A deletes B's token = %d, want 404", status)
	}
}

func TestHostedTokenAccountContainment(t *testing.T) {
	t.Parallel()
	app, api, accountID := hostedAccountServer(t)
	_, token := hostedMintToken(t, api, "contained", map[string]int{"client_keys": 1})
	use := func() int {
		status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil)
		return status
	}
	if use() != 200 {
		t.Fatal("token should work while active")
	}

	// Suspension takes effect immediately even though the token is cached.
	ctx := context.Background()
	if err := app.identity.SetAccountStatus(ctx, accountID, "suspended"); err != nil {
		t.Fatal(err)
	}
	app.tokens.InvalidateAccount(accountID)
	if got := use(); got != 401 {
		t.Fatalf("suspended account token = %d, want 401", got)
	}
	if err := app.identity.SetAccountStatus(ctx, accountID, "active"); err != nil {
		t.Fatal(err)
	}
	if got := use(); got != 200 {
		t.Fatalf("unsuspended token = %d, want 200", got)
	}

	// Deletion removes the tokens.
	if err := app.purgeAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	if got := use(); got != 401 {
		t.Fatalf("deleted account token = %d, want 401", got)
	}
	var n int
	if err := app.db.SQL.QueryRow(`SELECT COUNT(*) FROM api_tokens WHERE account_id=?`, accountID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tokens after purge = %d (%v), want 0", n, err)
	}
}

func TestHostedTokensSignOutEverywhereIsOptIn(t *testing.T) {
	t.Parallel()
	for _, revoke := range []bool{false, true} {
		_, api, _ := hostedAccountServer(t)
		_, token := hostedMintToken(t, api, "maybe", map[string]int{"client_keys": 1})
		if status, _, _ := api.request("POST", "/api/auth/account/sessions/revoke-all", map[string]any{"revoke_tokens": revoke}); status != 204 {
			t.Fatalf("revoke-all sessions = %d", status)
		}
		want := 200
		if revoke {
			want = 401
		}
		if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil); status != want {
			t.Fatalf("revoke_tokens=%v: token = %d, want %d", revoke, status, want)
		}
	}
}

// TestHostedSignOutTokenRevocationFailureIsReported proves a failed token
// delete is surfaced and does not sign the user out or claim success.
func TestHostedSignOutTokenRevocationFailureIsReported(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	_, token := hostedMintToken(t, api, "stubborn", map[string]int{"client_keys": 1})
	if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil); status != 200 {
		t.Fatal("token should work before the failure")
	}
	// Make the token delete fail while the rest of the database keeps working.
	if _, err := app.db.SQL.Exec(`CREATE TRIGGER block_token_delete BEFORE DELETE ON api_tokens BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.request("POST", "/api/auth/account/sessions/revoke-all", map[string]any{"revoke_tokens": true})
	if status != 500 || payload["error"].(map[string]any)["code"] != "token_revocation_failed" {
		t.Fatalf("revoke with failing delete: %d %v, want 500 token_revocation_failed", status, payload)
	}
	// The session is intact (not signed out) and the token still exists.
	if status, _, _ := api.request("GET", "/api/auth/account", nil); status != 200 {
		t.Fatalf("session after failed revoke = %d, want 200", status)
	}
	if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil); status != 200 {
		t.Fatalf("token after failed revoke = %d, want 200 (still valid, not claimed revoked)", status)
	}
	// Once the fault clears, a retry succeeds and the token dies.
	if _, err := app.db.SQL.Exec(`DROP TRIGGER block_token_delete`); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := api.request("POST", "/api/auth/account/sessions/revoke-all", map[string]any{"revoke_tokens": true}); status != 204 {
		t.Fatalf("retry = %d, want 204", status)
	}
	if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil); status != 401 {
		t.Fatalf("token after successful retry = %d, want 401", status)
	}
}

func TestHostedTokensSurvivePasswordReset(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	_, token := hostedMintToken(t, api, "keeps", map[string]int{"client_keys": 1})
	_, resetToken, err := app.identity.IssuePasswordReset(context.Background(), "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.requestWithHeaders("POST", "/api/auth/password-reset/confirm", map[string]any{"token": resetToken, "password": "a brand new password phrase"}, map[string]string{"Origin": "https://tiller.example.com"})
	if status != 200 {
		t.Fatalf("password reset: %d %v", status, payload)
	}
	if status, _, _ := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil); status != 200 {
		t.Fatalf("token after password reset = %d, want 200", status)
	}
}

func TestHostedTokenPerAccountRateLimit(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	app.dashboardLimiter = newWindowLimiter(3, time.Minute)
	_, tokenOne := hostedMintToken(t, api, "one", map[string]int{"client_keys": 1})
	_, tokenTwo := hostedMintToken(t, api, "two", map[string]int{"client_keys": 1})
	for i := 0; i < 3; i++ {
		tok := tokenOne
		if i%2 == 1 {
			tok = tokenTwo // the budget is shared across the account's tokens
		}
		if status, _, _ := bearerRequest(t, api.base, tok, "GET", "/api/admin/client-keys", nil); status != 200 {
			t.Fatalf("request %d = %d, want 200", i, status)
		}
	}
	status, payload, header := bearerRequest(t, api.base, tokenOne, "GET", "/api/admin/client-keys", nil)
	if status != 429 || header.Get("Retry-After") == "" {
		t.Fatalf("over budget = %d retry-after %q, want 429 with Retry-After", status, header.Get("Retry-After"))
	}
	retry, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || retry < 1 || retry > 60 {
		t.Fatalf("Retry-After = %q, want 1..60", header.Get("Retry-After"))
	}
	if header.Get("X-RateLimit-Limit") != "3" || header.Get("X-RateLimit-Remaining") != "0" || header.Get("X-RateLimit-Reset") != header.Get("Retry-After") {
		t.Fatalf("429 rate-limit headers = %v", header)
	}
	body := payload["error"].(map[string]any)
	if body["code"] != "rate_limited" || body["limit"] != float64(3) || body["window_seconds"] != float64(60) || body["retry_after"] != float64(retry) {
		t.Fatalf("429 body = %v", body)
	}
	// The browser session shares the account's budget: an account cannot get a
	// second allowance by switching credential, and a dashboard script driving
	// the same routes is throttled too.
	if status, _, _ := api.request("GET", "/api/admin/client-keys", nil); status != 429 {
		t.Fatalf("session request while the account budget is spent = %d, want 429", status)
	}
}

func TestHostedTokenRateLimitHeadersOnSuccess(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	app.dashboardLimiter = newWindowLimiter(5, time.Minute)
	_, token := hostedMintToken(t, api, "meter", map[string]int{"client_keys": 1})
	for i, want := range []string{"4", "3"} {
		status, _, header := bearerRequest(t, api.base, token, "GET", "/api/admin/client-keys", nil)
		if status != 200 || header.Get("X-RateLimit-Limit") != "5" || header.Get("X-RateLimit-Remaining") != want {
			t.Fatalf("request %d: %d limit=%q remaining=%q, want remaining %s", i, status, header.Get("X-RateLimit-Limit"), header.Get("X-RateLimit-Remaining"), want)
		}
		if reset, _ := strconv.Atoi(header.Get("X-RateLimit-Reset")); reset < 1 || reset > 60 {
			t.Fatalf("X-RateLimit-Reset = %q", header.Get("X-RateLimit-Reset"))
		}
	}
	// Headers also accompany scope refusals, since those requests are charged.
	if status, _, header := bearerRequest(t, api.base, token, "POST", "/api/admin/client-keys", map[string]any{"name": "x"}); status != 403 || header.Get("X-RateLimit-Remaining") != "2" {
		t.Fatalf("403 headers: %d remaining=%q", status, header.Get("X-RateLimit-Remaining"))
	}
}

func TestWindowLimiterResetsAtWindowEnd(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	l := newWindowLimiter(2, time.Minute)
	l.now = func() time.Time { return clock }
	if q := l.take("a"); !q.Allowed || q.Remaining != 1 || q.ResetSeconds != 60 {
		t.Fatalf("first = %+v", q)
	}
	clock = clock.Add(20 * time.Second)
	if q := l.take("a"); !q.Allowed || q.Remaining != 0 || q.ResetSeconds != 40 {
		t.Fatalf("second = %+v", q)
	}
	clock = clock.Add(10 * time.Second)
	if q := l.take("a"); q.Allowed || q.ResetSeconds != 30 {
		t.Fatalf("refused request = %+v, want refused with 30s left", q)
	}
	// Refusals do not extend the block: the window ends 60s after the first request.
	clock = clock.Add(30 * time.Second)
	if q := l.take("a"); !q.Allowed || q.Remaining != 1 {
		t.Fatalf("after window = %+v, want a fresh budget", q)
	}
	if q := l.take("b"); !q.Allowed || q.Remaining != 1 {
		t.Fatalf("other key = %+v", q)
	}
}

func TestHostedTokenCreationQueuesNotice(t *testing.T) {
	t.Parallel()
	app, api, _ := hostedAccountServer(t)
	hostedMintToken(t, api, "noticed", map[string]int{})
	var n int
	if err := app.db.SQL.QueryRow(`SELECT COUNT(*) FROM mail_outbox WHERE type='api_token_created' AND recipient='owner@example.com'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("queued notices = %d (%v), want 1", n, err)
	}
}

func TestHostedSeedKeyStillRejected(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	// The environment-managed id is not creatable or mutable through the API.
	if status, _, _ := api.request("DELETE", "/api/admin/api-tokens/"+bootManagementTokenID, nil); status != 409 {
		t.Fatalf("boot token delete = %d, want 409", status)
	}
}

func storeCreateToken(id, name string, g auth.GeneratedKey, scopes string) store.CreateManagementTokenInput {
	return store.CreateManagementTokenInput{ID: id, Name: name, Selector: g.Selector, Hash: g.Hash, Fingerprint: g.Fingerprint, Scopes: scopes}
}

// hostedSecondTenant creates an independent verified account on the same
// server and logs it in, returning a cookie session for it.
func hostedSecondTenant(t *testing.T, api *testAPI, email, password string) *testAPI {
	t.Helper()
	ctx := context.Background()
	result, err := api.server.identity.CreateSignup(ctx, email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.server.identity.ConsumeVerification(ctx, result.VerificationToken, password); err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	other := &testAPI{t: t, base: api.base, client: &http.Client{Jar: jar}, server: api.server}
	status, payload, _ := other.request("POST", "/api/auth/login", map[string]any{"email": email, "password": password})
	if status != 200 {
		t.Fatalf("second tenant login: %d %v", status, payload)
	}
	other.csrf = payload["csrf_token"].(string)
	return other
}

// TestRevokeAllIsTenantScoped proves one tenant's revoke-all leaves another
// tenant's API tokens authenticating exactly as before. The per-account cache
// drop must not wipe (or fail closed on) anyone else's verified tokens.
func TestRevokeAllIsTenantScoped(t *testing.T) {
	t.Parallel()
	_, apiA, _ := hostedAccountServer(t)
	_, tokenA := hostedMintToken(t, apiA, "tenant-a", map[string]int{"client_keys": 1})

	const tenantBPassword = "another correct horse"
	apiB := hostedSecondTenant(t, apiA, "tenant-b@example.com", tenantBPassword)
	_, tokenB := hostedMintToken(t, apiB, "tenant-b", map[string]int{"client_keys": 1})

	useB := func() int {
		status, _, _ := bearerRequest(t, apiB.base, tokenB, "GET", "/api/admin/client-keys", nil)
		return status
	}
	// Twice, so tenant B's entry is warm in the verification cache: the defect
	// this guards against is a global cache wipe, which only hurts a cached
	// token.
	if useB() != 200 || useB() != 200 {
		t.Fatal("tenant B token should work before tenant A revokes")
	}

	if status, payload, _ := apiA.request("POST", "/api/admin/api-tokens/revoke-all", map[string]any{}); status != 200 {
		t.Fatalf("tenant A revoke-all: %d %v", status, payload)
	}

	if status, _, _ := bearerRequest(t, apiA.base, tokenA, "GET", "/api/admin/client-keys", nil); status != 401 {
		t.Fatalf("tenant A token after revoke-all = %d, want 401", status)
	}
	if got := useB(); got != 200 {
		t.Fatalf("tenant B token after tenant A revoke-all = %d, want 200", got)
	}
}
