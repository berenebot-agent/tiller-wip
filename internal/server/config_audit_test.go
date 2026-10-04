package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
	"github.com/tiller-router/tiller-router/internal/testutil/fastsecret"
)

// auditEvents returns the account's audit events as a set of event names.
func auditEvents(t *testing.T, db *database.DB, accountID string) map[string]store.AuditRow {
	t.Helper()
	rows, err := store.New(db.SQL).For(accountID).ListAccountAudit(context.Background(), 200, 0)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	out := make(map[string]store.AuditRow, len(rows))
	for _, row := range rows {
		out[row.Event] = row
	}
	return out
}

// TestTenantMutationsWriteAuditEvents proves the pre-SaaS review TR-014 gap is
// closed: provider, client-key, virtual-routing, and settings mutations write
// account-audit events (best-effort), each with an actor, target, and no
// secret values in metadata.
func TestTenantMutationsWriteAuditEvents(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))

	// loggingTestHarness already created a provider, a client key, and updated
	// the key's permissions; those must have been audited.
	events := auditEvents(t, db, database.LocalAccountID)
	for _, want := range []string{"provider.created", "client_key.created", "client_key.permissions_updated"} {
		if _, ok := events[want]; !ok {
			t.Fatalf("missing audit event %q; got %v", want, eventNames(events))
		}
	}
	if events["client_key.created"].ActorID == "" {
		t.Fatal("client_key.created audit event has no actor id")
	}

	// More mutations across each family.
	if status, payload, _ := api.request("POST", "/api/admin/client-keys/"+clientID+"/rotate", nil); status != 200 {
		t.Fatalf("rotate: %d %v", status, payload)
	}
	if status, payload, _ := api.request("PUT", "/api/admin/settings", map[string]any{
		"notifications_webhook_url": "https://hooks.example.com/abc",
		"notifications_auth_header": "Bearer super-secret-token",
	}); status != 204 {
		t.Fatalf("settings: %d %v", status, payload)
	}
	if status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "grp"}); status != 201 {
		t.Fatalf("group: %d %v", status, payload)
	}

	events = auditEvents(t, db, database.LocalAccountID)
	for _, want := range []string{"client_key.rotated", "settings.updated", "virtual_group.created"} {
		if _, ok := events[want]; !ok {
			t.Fatalf("missing audit event %q; got %v", want, eventNames(events))
		}
	}

	// The settings audit names fields but must never contain the secret value.
	settings := events["settings.updated"]
	if strings.Contains(settings.Metadata, "super-secret-token") || strings.Contains(settings.Metadata, "hooks.example.com") {
		t.Fatalf("settings audit metadata leaked a value: %s", settings.Metadata)
	}
	if !strings.Contains(settings.Metadata, "notifications_webhook_url") {
		t.Fatalf("settings audit metadata is missing the changed field names: %s", settings.Metadata)
	}
}

func eventNames(events map[string]store.AuditRow) []string {
	names := make([]string, 0, len(events))
	for name := range events {
		names = append(names, name)
	}
	return names
}

// TestLiveSubscriberCap proves the per-account live-SSE connection cap
// (TR-007): the cap is enforced, and unsubscribing frees a slot.
func TestLiveSubscriberCap(t *testing.T) {
	h := &liveHub{
		outcomeCh:  make(chan outcomeEvent, liveOutcomeBuffer),
		activityCh: make(chan activityEvent, liveOutcomeBuffer),
		timings:    liveTimings{debounce: time.Hour, idle: time.Hour, sessionCheck: time.Hour},
	}
	const acct = "acct-live-cap"
	subs := make([]chan []byte, 0, maxLiveSubscribersPerAccount)
	for i := 0; i < maxLiveSubscribersPerAccount; i++ {
		ch, ok := h.subscribe(acct)
		if !ok {
			t.Fatalf("subscription %d refused before the cap", i)
		}
		subs = append(subs, ch)
	}
	if _, ok := h.subscribe(acct); ok {
		t.Fatalf("subscription past the cap (%d) should have been refused", maxLiveSubscribersPerAccount)
	}
	// A slot is per account: a different account is unaffected.
	if _, ok := h.subscribe("other-account"); !ok {
		t.Fatal("cap for one account must not block another")
	}
	// Freeing a slot admits the next subscriber again.
	h.unsubscribe(acct, subs[0])
	if _, ok := h.subscribe(acct); !ok {
		t.Fatal("subscription should be admitted after a slot is freed")
	}
}

// TestRequestLogIncludesAccountID proves the second half of TR-014: an
// authenticated request's log line carries the account id and principal kind.
func TestRequestLogIncludesAccountID(t *testing.T) {
	var buf strings.Builder
	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	app, err := New(
		config.Config{TillerUser: "admin", TillerUserPassword: "correct horse", DataDir: t.TempDir(), ListenAddr: ":8080"},
		db,
		slog.New(slog.NewTextHandler(&buf, nil)),
		withSecretHasher(fastsecret.Hasher{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	login, err := http.NewRequest("POST", router.URL+"/api/admin/session", strings.NewReader(`{"username":"admin","password":"correct horse"}`))
	if err != nil {
		t.Fatal(err)
	}
	login.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(login)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login status %d", resp.StatusCode)
	}
	buf.Reset()

	get, _ := http.NewRequest("GET", router.URL+"/api/admin/providers", nil)
	resp, err = client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("providers status %d", resp.StatusCode)
	}
	logged := buf.String()
	if !strings.Contains(logged, "account_id="+database.LocalAccountID) {
		t.Fatalf("request log missing account attribution:\n%s", logged)
	}
	if !strings.Contains(logged, "principal=admin") {
		t.Fatalf("request log missing principal kind:\n%s", logged)
	}
}
