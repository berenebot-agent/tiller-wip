package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// TestPasskeyFinishBodySelectsGateByPrincipal pins the 2026-11-14 review's last
// finding: passkeyFinishBody charged the anonymous pre-auth pool for every
// caller, so an unauthenticated flood on the sign-in finish route refused a
// signed-in tenant's passkey registration and re-authentication. The gate now
// follows the principal.
func TestPasskeyFinishBodySelectsGateByPrincipal(t *testing.T) {
	t.Parallel()
	app, _ := newSecurityTestServer(t, config.Config{
		TillerUser: "admin", TillerUserPassword: "correct horse",
		DataDir: t.TempDir(), ListenAddr: ":8080",
	})
	finish := func(accountID string) (*httptest.ResponseRecorder, bool) {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "http://router.test/api/auth/account/passkeys/register/finish",
			strings.NewReader(`{"id":"x"}`))
		r.Header.Set("Content-Type", "application/json")
		if accountID != "" {
			r = r.WithContext(context.WithValue(r.Context(), accountKey, accountID))
		}
		return rec, app.passkeyFinishBody(rec, r)
	}

	// Saturate the anonymous pre-auth pool: an anonymous finish is refused, an
	// authenticated one is still admitted.
	for i := 0; i < maxConcurrentPreAuthBodyReads; i++ {
		if !app.preAuthBodyReads.acquire("") {
			t.Fatalf("pre-auth pool refused at %d", i)
		}
	}
	if rec, ok := finish(""); ok || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("anonymous finish with the pre-auth pool full = ok:%v code:%d, want refused 503", ok, rec.Code)
	}
	if _, ok := finish(database.LocalAccountID); !ok {
		t.Fatal("authenticated finish was refused while only the anonymous pool was full")
	}
	for i := 0; i < maxConcurrentPreAuthBodyReads; i++ {
		app.preAuthBodyReads.release("")
	}

	// Saturate the account's inference share: the authenticated finish is now
	// refused, and the anonymous one is admitted again.
	key := database.LocalAccountID
	for i := 0; i < maxConcurrentBodyReadsPerAccount; i++ {
		app.bodyReads.acquire(key)
	}
	if rec, ok := finish(key); ok || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("authenticated finish with the account share full = ok:%v code:%d, want refused 503", ok, rec.Code)
	}
	if _, ok := finish(""); !ok {
		t.Fatal("anonymous finish was refused while only an account's share was full")
	}
	for i := 0; i < maxConcurrentBodyReadsPerAccount; i++ {
		app.bodyReads.release(key)
	}
}

// TestAuthSurfaceDecoderTakesBodyGate pins the fix for the 2026-11-14 retest's
// finding: the auth-surface decoder family (decodeJSONLimit) took no slot in
// any admission gate, so authenticated account/passkey handlers — and the
// unauthenticated login, signup and reset handlers — could park unbounded
// partial bodies outside the bound every other body-reading route respects.
//
// decodeAuthJSON charges the account's inference share for an authenticated
// request and a dedicated pre-auth pool for an anonymous one.
func TestAuthSurfaceDecoderTakesBodyGate(t *testing.T) {
	t.Parallel()
	app, _ := newSecurityTestServer(t, config.Config{
		TillerUser: "admin", TillerUserPassword: "correct horse",
		DataDir: t.TempDir(), ListenAddr: ":8080",
	})

	decode := func(r *http.Request) error {
		var payload struct {
			X string `json:"x"`
		}
		return app.decodeAuthJSON(httptest.NewRecorder(), r, &payload, authRequestMaxBytes)
	}
	request := func(accountID string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://router.test/api/admin/session",
			strings.NewReader(`{"x":"y"}`))
		r.Header.Set("Content-Type", "application/json")
		if accountID != "" {
			r = r.WithContext(context.WithValue(r.Context(), accountKey, accountID))
		}
		return r
	}

	// Anonymous: the dedicated auth pool applies, and inference capacity is
	// untouched.
	for i := 0; i < maxConcurrentAuthBodyReads; i++ {
		if !app.authBodyReads.acquire("") {
			t.Fatalf("auth pool refused at %d, before its cap", i)
		}
	}
	if err := decode(request("")); !errors.Is(err, errBodyReadBusy) {
		t.Fatalf("anonymous decode with the auth pool full = %v, want errBodyReadBusy", err)
	}
	if n := app.bodyReads.inUse(); n != 0 {
		t.Fatalf("anonymous auth decode charged %d inference slots, want 0", n)
	}
	for i := 0; i < maxConcurrentAuthBodyReads; i++ {
		app.authBodyReads.release("")
	}

	// Authenticated: the account's inference share applies.
	key := database.LocalAccountID
	for i := 0; i < maxConcurrentBodyReadsPerAccount; i++ {
		if !app.bodyReads.acquire(key) {
			t.Fatalf("account share refused at %d, before its cap", i)
		}
	}
	if err := decode(request(key)); !errors.Is(err, errBodyReadBusy) {
		t.Fatalf("authenticated decode with the account share full = %v, want errBodyReadBusy", err)
	}
	for i := 0; i < maxConcurrentBodyReadsPerAccount; i++ {
		app.bodyReads.release(key)
	}

	// Operator console: its own plane. Saturating the anonymous auth pool must not
	// refuse a console write, and the console pool refuses on its own.
	platformReq := func() *http.Request {
		r := request("")
		return r.WithContext(context.WithValue(r.Context(), platformListenerContextKey{}, true))
	}
	// Re-saturate the anonymous pool so the next assertion is a real separation
	// test: a full anonymous pool must not refuse a console write.
	for i := 0; i < maxConcurrentAuthBodyReads; i++ {
		if !app.authBodyReads.acquire("") {
			t.Fatalf("auth pool refused at %d while re-saturating", i)
		}
	}
	if err := decode(platformReq()); err != nil {
		t.Fatalf("platform decode with the ANONYMOUS pool full = %v, want admitted", err)
	}
	for i := 0; i < maxConcurrentAuthBodyReads; i++ {
		app.authBodyReads.release("")
	}
	for i := 0; i < maxConcurrentPlatformBodyReads; i++ {
		if !app.platformBodyReads.acquire("") {
			t.Fatalf("platform pool refused at %d, before its cap", i)
		}
	}
	if err := decode(platformReq()); !errors.Is(err, errBodyReadBusy) {
		t.Fatalf("platform decode with the platform pool full = %v, want errBodyReadBusy", err)
	}
	if n := app.authBodyReads.inUse(); n != 0 {
		t.Fatalf("platform decode charged %d anonymous auth slots, want 0", n)
	}
	for i := 0; i < maxConcurrentPlatformBodyReads; i++ {
		app.platformBodyReads.release("")
	}

	// With capacity free both paths decode normally.
	if err := decode(request("")); err != nil {
		t.Fatalf("anonymous decode with free capacity = %v", err)
	}
	if err := decode(request(key)); err != nil {
		t.Fatalf("authenticated decode with free capacity = %v", err)
	}
}

// TestLoginRefusesWhenAuthPoolFull is the end-to-end half: a real HTTP login
// whose body read arrives while the auth pool is saturated must be refused with
// the shared busier code rather than being decoded anyway. It fails on the
// pre-fix decoder, which returned the credential error instead.
func TestLoginRefusesWhenAuthPoolFull(t *testing.T) {
	t.Parallel()
	app, _ := newSecurityTestServer(t, config.Config{
		TillerUser: "admin", TillerUserPassword: "correct horse",
		DataDir: t.TempDir(), ListenAddr: ":8080",
	})
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	post := func() (*http.Response, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, router.URL+"/api/admin/session",
			strings.NewReader(`{"username":"admin","password":"wrong"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, payload
	}

	for i := 0; i < maxConcurrentAuthBodyReads; i++ {
		if !app.authBodyReads.acquire("") {
			t.Fatalf("auth pool refused at %d", i)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < maxConcurrentAuthBodyReads; i++ {
			app.authBodyReads.release("")
		}
	})

	resp, payload := post()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("login with the auth pool full = %d %s, want 503", resp.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "body_read_busy") {
		t.Fatalf("refusal code = %s, want body_read_busy", payload)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("busy refusal carried no Retry-After header")
	}
}
