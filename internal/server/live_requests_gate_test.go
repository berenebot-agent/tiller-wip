package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// TestReconcileLiveRequestsConsumesBodyGate pins the fix for the 2026-11-14
// review's vuln-0001: the reconcile route used a hand-rolled decoder and so sat
// outside the per-account body-read admission gate that every other console
// write takes. An authenticated tenant could park half-sent bodies on it with
// no refusal, while its gated siblings refused the next body once the account's
// share was full.
//
// The test drives the real router (so the gate key comes from the authenticated
// principal, not from a hand-set context), proves the route still works with the
// gate free, then saturates the account's share and requires the shared refusal.
func TestReconcileLiveRequestsConsumesBodyGate(t *testing.T) {
	t.Parallel()
	app, _ := newSecurityTestServer(t, config.Config{
		TillerUser: "admin", TillerUserPassword: "correct horse",
		DataDir: t.TempDir(), ListenAddr: ":8080",
	})
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)

	do := func(path, body, cookie, csrf string) (*http.Response, []byte) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(http.MethodPost, router.URL+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if cookie != "" {
			req.Header.Set("Cookie", sessionCookie+"="+cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := router.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		payload, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, payload
	}

	login, payload := do("/api/admin/session", `{"username":"admin","password":"correct horse"}`, "", "")
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login = %d %s", login.StatusCode, payload)
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(payload, &session); err != nil || session.CSRF == "" {
		t.Fatalf("login payload = %s (err %v)", payload, err)
	}
	cookie := ""
	for _, c := range login.Cookies() {
		if c.Name == sessionCookie {
			cookie = c.Value
		}
	}
	if cookie == "" {
		t.Fatal("login set no session cookie")
	}

	// Control: with the gate free the route behaves exactly as before.
	resp, payload := do("/api/admin/live/requests/reconcile", `{"ids":[]}`, cookie, session.CSRF)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reconcile with a free gate = %d %s", resp.StatusCode, payload)
	}

	// The two pre-existing rejections survive the decoder swap.
	resp, _ = do("/api/admin/live/requests/reconcile", `{"ids":`, cookie, session.CSRF)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", resp.StatusCode)
	}
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = "r"
	}
	tooMany, _ := json.Marshal(map[string]any{"ids": ids})
	resp, _ = do("/api/admin/live/requests/reconcile", string(tooMany), cookie, session.CSRF)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("1001 ids = %d, want 400", resp.StatusCode)
	}

	// Saturate this account's share of the gate. A route that does not take a
	// slot answers 200 here, which is the defect this test exists to catch.
	key := database.LocalAccountID
	for i := 0; i < maxConcurrentBodyReadsPerAccount; i++ {
		if !app.bodyReads.acquire(key) {
			t.Fatalf("gate refused at %d, before the per-account share", i)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < maxConcurrentBodyReadsPerAccount; i++ {
			app.bodyReads.release(key)
		}
	})

	resp, payload = do("/api/admin/live/requests/reconcile", `{"ids":[]}`, cookie, session.CSRF)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("reconcile with the gate saturated = %d %s, want 503", resp.StatusCode, payload)
	}
	if !strings.Contains(string(payload), "body_read_busy") {
		t.Fatalf("refusal code = %s, want body_read_busy", payload)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("busy refusal carried no Retry-After header")
	}

	// The gated sibling must agree, which is what makes this one invariant
	// rather than a per-route assertion.
	resp, payload = do("/api/admin/api-tokens", `{"name":"probe","scopes":{"monitoring":1}}`, cookie, session.CSRF)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(payload), "body_read_busy") {
		t.Fatalf("gated sibling with the gate saturated = %d %s, want 503 body_read_busy", resp.StatusCode, payload)
	}
}
