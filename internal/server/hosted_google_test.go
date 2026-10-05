package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/hostedauth"
	"github.com/tiller-router/tiller-router/internal/identity"
	"github.com/tiller-router/tiller-router/internal/store"
)

// rawUserSessionTokenFromJar reads the current hosted user-session cookie value
// from an authenticated test client, the same raw token the handlers pass to
// reauthentication and beginning a Google flow.
func rawUserSessionTokenFromJar(t *testing.T, api *testAPI) string {
	t.Helper()
	base, err := url.Parse(api.base)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range api.client.Jar.Cookies(base) {
		if c.Name == userSessionCookie {
			return c.Value
		}
	}
	return ""
}

// seedGooglePlatformSettings enables Google sign-in so the completion handlers
// pass their platform-settings guard. The verifier is not consulted: these
// tests drive the completion handlers with claims directly, which is where the
// authority decision and the link gate live.
func seedGooglePlatformSettings(t *testing.T, app *Server) {
	t.Helper()
	ctx := context.Background()
	for key, value := range map[string]string{
		store.PlatformSettingGoogleEnabled:      "1",
		store.PlatformSettingGoogleClientID:     "test-client.apps.googleusercontent.com",
		store.PlatformSettingGoogleClientSecret: "test-secret",
	} {
		if err := app.storeHandle().SetPlatformSetting(ctx, key, value); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
}

// seedPendingGoogleClaim stores a validated Google identity the way the sign-in
// handlers do, and returns the cookie value that completion consumes.
func seedPendingGoogleClaim(t *testing.T, app *Server, claims hostedauth.SignupClaims) string {
	t.Helper()
	token, ok := app.googlePending.Put(claims)
	if !ok {
		t.Fatal("store pending claim")
	}
	return token
}

func setGoogleSignupCookie(t *testing.T, api *testAPI, value string) {
	t.Helper()
	base, err := url.Parse(api.base)
	if err != nil {
		t.Fatal(err)
	}
	api.client.Jar.SetCookies(base, []*http.Cookie{{Name: googleSignupCookie, Value: value}})
}

// googleIdentityCount reports how many Google identities are attached to an
// account's owner user.
func googleIdentityCount(t *testing.T, app *Server, accountID string) int {
	t.Helper()
	var n int
	err := app.db.SQL.QueryRowContext(context.Background(),
		`SELECT count(*) FROM user_identities i JOIN users u ON u.id=i.user_id JOIN accounts a ON a.owner_user_id=u.id WHERE a.id=? AND i.provider='google'`, accountID).Scan(&n)
	if err != nil {
		t.Fatalf("count google identities: %v", err)
	}
	return n
}

func TestGoogleSignupCompletionRejectsThirdPartyLink(t *testing.T) {
	app, api, accountID := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)

	cookie := seedPendingGoogleClaim(t, app, hostedauth.SignupClaims{
		Subject: "google-subject-thirdparty", Email: "owner@example.com", Authoritative: false,
	})
	setGoogleSignupCookie(t, api, cookie)

	status, payload, _ := api.request("POST", "/api/auth/google/signup/complete", map[string]any{"link_existing": true})
	if status != http.StatusConflict {
		t.Fatalf("third-party link_existing = %d %v, want 409", status, payload)
	}
	if code := errorCode(payload); code != "google_link_challenge_required" {
		t.Fatalf("error code = %q, want google_link_challenge_required", code)
	}
	if n := googleIdentityCount(t, app, accountID); n != 0 {
		t.Fatalf("google identities attached = %d, want 0", n)
	}
}

func TestGoogleSignupCompletionAllowsAuthoritativeLink(t *testing.T) {
	app, api, accountID := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)

	cookie := seedPendingGoogleClaim(t, app, hostedauth.SignupClaims{
		Subject: "google-subject-gmail", Email: "owner@example.com", Authoritative: true,
	})
	setGoogleSignupCookie(t, api, cookie)

	status, payload, _ := api.request("POST", "/api/auth/google/signup/complete", map[string]any{"link_existing": true})
	if status != http.StatusOK {
		t.Fatalf("authoritative link_existing = %d %v, want 200", status, payload)
	}
	if payload["authenticated"] != true {
		t.Fatalf("link response carried no session: %v", payload)
	}
	if n := googleIdentityCount(t, app, accountID); n != 1 {
		t.Fatalf("google identities attached = %d, want 1", n)
	}
}

func TestGoogleSignupCompletionRequiresTermsForNewAccount(t *testing.T) {
	app, api, _ := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)
	if err := app.storeHandle().SetHostedSignupEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := app.SeedLegalDocuments(context.Background()); err != nil {
		t.Fatal(err)
	}

	cookie := seedPendingGoogleClaim(t, app, hostedauth.SignupClaims{
		Subject: "google-subject-new", Email: "newcomer@example.com", Authoritative: true,
	})
	setGoogleSignupCookie(t, api, cookie)

	status, payload, _ := api.request("POST", "/api/auth/google/signup/complete", map[string]any{"accept_terms": false})
	if status != http.StatusBadRequest {
		t.Fatalf("signup without terms = %d %v, want 400", status, payload)
	}
	if code := errorCode(payload); code != "terms_not_accepted" {
		t.Fatalf("error code = %q, want terms_not_accepted", code)
	}
	if _, err := app.identity.UserByEmail(context.Background(), "newcomer@example.com"); err == nil {
		t.Fatal("account was created despite missing terms acceptance")
	}
}

func TestGoogleSignupCompletionCreatesAccountWithTerms(t *testing.T) {
	app, api, _ := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)
	if err := app.storeHandle().SetHostedSignupEnabled(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := app.SeedLegalDocuments(context.Background()); err != nil {
		t.Fatal(err)
	}

	cookie := seedPendingGoogleClaim(t, app, hostedauth.SignupClaims{
		Subject: "google-subject-new2", Email: "newcomer2@example.com", Authoritative: true,
	})
	setGoogleSignupCookie(t, api, cookie)

	status, payload, _ := api.request("POST", "/api/auth/google/signup/complete", map[string]any{"accept_terms": true})
	if status != http.StatusOK {
		t.Fatalf("signup with terms = %d %v, want 200", status, payload)
	}
	if payload["authenticated"] != true {
		t.Fatalf("signup response carried no session: %v", payload)
	}
}

// TestGoogleSignupCompletionKeepsClaimOnRefusal protects the Peek-not-Take
// contract: a refused attempt must leave the claim intact so the visitor can
// retry without another Google round trip.
func TestGoogleSignupCompletionKeepsClaimOnRefusal(t *testing.T) {
	app, api, _ := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)

	cookie := seedPendingGoogleClaim(t, app, hostedauth.SignupClaims{
		Subject: "google-subject-retry", Email: "owner@example.com", Authoritative: false,
	})
	setGoogleSignupCookie(t, api, cookie)

	if status, _, _ := api.request("POST", "/api/auth/google/signup/complete", map[string]any{"link_existing": true}); status != http.StatusConflict {
		t.Fatalf("first refusal = %d, want 409", status)
	}
	if _, ok := app.googlePending.Peek(cookie); !ok {
		t.Fatal("pending claim was consumed by a refused attempt")
	}
	status, payload, _ := api.request("POST", "/api/auth/google/signup/complete", map[string]any{"link_existing": true})
	if status != http.StatusConflict {
		t.Fatalf("second attempt = %d %v, want 409 (not expired)", status, payload)
	}
	if code := errorCode(payload); code != "google_link_challenge_required" {
		t.Fatalf("second error code = %q, want google_link_challenge_required", code)
	}
}

// TestCompleteGoogleLinkIssuesLiveSession is the regression for the dead-cookie
// defect: linking revokes the initiating session, so the callback must hand the
// browser a NEW session whose row actually exists.
func TestCompleteGoogleLinkIssuesLiveSession(t *testing.T) {
	app, api, _ := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)
	sessionToken := rawUserSessionTokenFromJar(t, api)
	if sessionToken == "" {
		t.Fatal("no session token in jar")
	}

	flow := hostedauth.Flow{
		State: "state-1", Nonce: "nonce-1", Verifier: "verifier-1",
		Intent: hostedauth.IntentLink, SessionToken: sessionToken, ExpiresAt: time.Now().Add(time.Minute),
	}
	claims := hostedauth.GoogleIdentity{Subject: "google-subject-link", Email: "owner@example.com", EmailVerified: true}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/google/callback", nil)
	req.AddCookie(&http.Cookie{Name: userSessionCookie, Value: sessionToken})
	app.completeGoogleLink(rec, req, flow, claims)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("completeGoogleLink status = %d, want 303 (body %s)", rec.Code, rec.Body.String())
	}
	fresh := sessionCookieValue(rec.Result().Cookies(), userSessionCookie)
	if fresh == "" {
		t.Fatal("no fresh session cookie issued")
	}
	if fresh == sessionToken {
		t.Fatal("callback re-issued the revoked session token")
	}
	if _, ok := app.identity.GetUserSession(context.Background(), fresh); !ok {
		t.Fatal("issued session cookie does not resolve to a live session")
	}
}

// TestPendingGoogleLinkGrantAllowsLinkWithoutPassword drives the interstitial
// path end to end at the HTTP layer: a password login carrying a pending Google
// claim returns the flag and a grant, and startGoogleLink accepts an empty
// password.
func TestPendingGoogleLinkGrantAllowsLinkWithoutPassword(t *testing.T) {
	app, _, _ := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)

	// A second, unlinked, password-enabled account.
	u, err := app.identity.CreateSignup(context.Background(), "candidate@example.com", "another correct horse battery", identity.SignupAcceptance{})
	if err != nil {
		t.Fatalf("create candidate: %v", err)
	}
	if _, err := app.identity.ConsumeVerification(context.Background(), u.VerificationToken); err != nil {
		t.Fatalf("verify candidate: %v", err)
	}

	cookie := seedPendingGoogleClaim(t, app, hostedauth.SignupClaims{
		Subject: "google-subject-candidate", Email: "candidate@example.com", Authoritative: false,
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	base, _ := url.Parse(api.base)
	api.client.Jar.SetCookies(base, []*http.Cookie{{Name: googleSignupCookie, Value: cookie}})

	status, payload, _ := api.request("POST", "/api/auth/login", map[string]any{"email": "candidate@example.com", "password": "another correct horse battery"})
	if status != http.StatusOK {
		t.Fatalf("candidate login = %d %v", status, payload)
	}
	api.csrf = payload["csrf_token"].(string)
	if payload["pending_google_link"] != true {
		t.Fatalf("pending_google_link = %v, want true", payload["pending_google_link"])
	}

	status, payload, _ = api.request("POST", "/api/auth/google/link/start", map[string]any{"current_password": ""})
	if status != http.StatusOK {
		t.Fatalf("link/start with grant = %d %v, want 200", status, payload)
	}
	if redirect, _ := payload["redirect_url"].(string); redirect == "" {
		t.Fatalf("link/start returned no redirect_url: %v", payload)
	}
}

// TestStartGoogleLinkRequiresPasswordWithoutGrant guards against the grant
// accidentally removing the password requirement for ordinary sessions.
func TestStartGoogleLinkRequiresPasswordWithoutGrant(t *testing.T) {
	app, api, _ := hostedAccountServer(t)
	seedGooglePlatformSettings(t, app)

	status, payload, _ := api.request("POST", "/api/auth/google/link/start", map[string]any{"current_password": "wrong password entirely"})
	if status != http.StatusUnauthorized {
		t.Fatalf("link/start without grant and wrong password = %d %v, want 401", status, payload)
	}
}

func sessionCookieValue(cookies []*http.Cookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}
