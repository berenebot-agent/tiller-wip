package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/store"
)

// bodyLogUpstream is a mock provider that always fails, so a failed request
// produces the row the detailed-body logging governs.
func bodyLogUpstream(t *testing.T) http.HandlerFunc {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "model-a"}}})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "PROVIDER-ERROR-SECRET-MARKER"}})
	})
}

// setBodyLogMode patches a client key's detailed-error-body mode.
func setBodyLogMode(t *testing.T, api *testAPI, clientID, mode string) int {
	t.Helper()
	status, payload, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": mode})
	if status != http.StatusNoContent {
		t.Fatalf("set body log mode %q: %d %v", mode, status, payload)
	}
	return status
}

// setAccountBodyLogging switches the account-level detailed-error-logging
// setting on or off.
func setAccountBodyLogging(t *testing.T, db *database.DB, enabled bool) {
	t.Helper()
	value := "0"
	if enabled {
		value = "1"
	}
	if err := store.New(db.SQL).For(database.LocalAccountID).SetSetting(context.Background(), store.SettingLogErrorBodies, value); err != nil {
		t.Fatal(err)
	}
}

// loggedBodies reads the persisted request and provider error bodies for the
// most recent request of a client key.
func loggedBodies(t *testing.T, db *database.DB, clientID string) (int, *string, *string) {
	t.Helper()
	var status int
	var requestBody, errorBody *string
	if err := activityDB(t, db).QueryRow(`SELECT http_status,request_body,error_body FROM request_logs WHERE client_key_id=? ORDER BY created_at DESC LIMIT 1`, clientID).Scan(&status, &requestBody, &errorBody); err != nil {
		t.Fatal(err)
	}
	return status, requestBody, errorBody
}

// failRequest drives one failing request through the router.
func failRequest(t *testing.T, api *testAPI, secret string) {
	t.Helper()
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "CLIENT-REQUEST-SECRET-MARKER"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("provider error status = %d", resp.StatusCode)
	}
}

// TestClientBodyLogNeverKeepsMetadataButDropsBodies pins the per-client
// "never" mode: the request is still logged as metadata, but no body content
// is stored even though the account setting allows it.
func TestClientBodyLogNeverKeepsMetadataButDropsBodies(t *testing.T) {
	t.Parallel()
	api, db, clientID, secret := loggingTestHarness(t, bodyLogUpstream(t))
	setAccountBodyLogging(t, db, true)
	setBodyLogMode(t, api, clientID, store.BodyLogNever)

	failRequest(t, api, secret)

	status, requestBody, errorBody := loggedBodies(t, db, clientID)
	if status != http.StatusBadGateway {
		t.Fatalf("activity row status = %d", status)
	}
	if requestBody != nil || errorBody != nil {
		t.Fatalf("bodies persisted for a client set to never: request=%v error=%v", requestBody, errorBody)
	}
}

// TestClientBodyLogAlwaysCapturesBodiesWithoutAccountSetting pins the
// per-client "always" mode: bodies are stored even when the account setting
// is off.
func TestClientBodyLogAlwaysCapturesBodiesWithoutAccountSetting(t *testing.T) {
	t.Parallel()
	api, db, clientID, secret := loggingTestHarness(t, bodyLogUpstream(t))
	setAccountBodyLogging(t, db, false)
	setBodyLogMode(t, api, clientID, store.BodyLogAlways)

	failRequest(t, api, secret)

	_, requestBody, errorBody := loggedBodies(t, db, clientID)
	if requestBody == nil || !strings.Contains(*requestBody, "CLIENT-REQUEST-SECRET-MARKER") {
		t.Fatalf("request body was not persisted for a client set to always: %v", requestBody)
	}
	if errorBody == nil || !strings.Contains(*errorBody, "PROVIDER-ERROR-SECRET-MARKER") {
		t.Fatalf("provider error body was not persisted for a client set to always: %v", errorBody)
	}
}

// TestClientBodyLogInheritFollowsAccountSetting pins the default mode: with
// no per-client choice, the account setting decides.
func TestClientBodyLogInheritFollowsAccountSetting(t *testing.T) {
	t.Parallel()
	api, db, clientID, secret := loggingTestHarness(t, bodyLogUpstream(t))
	setAccountBodyLogging(t, db, false)

	failRequest(t, api, secret)

	_, requestBody, errorBody := loggedBodies(t, db, clientID)
	if requestBody != nil || errorBody != nil {
		t.Fatalf("bodies persisted while the account setting is off: request=%v error=%v", requestBody, errorBody)
	}
}

// TestClientBodyLogModeAPI covers the admin surface: the mode round-trips
// and an unknown mode is rejected.
func TestClientBodyLogModeAPI(t *testing.T) {
	t.Parallel()
	api, _, clientID, _ := loggingTestHarness(t, bodyLogUpstream(t))

	for _, mode := range []string{store.BodyLogNever, store.BodyLogAlways, store.BodyLogInherit} {
		setBodyLogMode(t, api, clientID, mode)
		status, payload, _ := api.request("GET", "/api/admin/client-keys", nil)
		if status != http.StatusOK {
			t.Fatalf("list client keys: %d %v", status, payload)
		}
		rows := payload["data"].([]any)
		if len(rows) != 1 {
			t.Fatalf("expected one client key, got %d", len(rows))
		}
		if got := rows[0].(map[string]any)["log_error_bodies"]; got != mode {
			t.Fatalf("client mode = %v, want %q", got, mode)
		}
	}

	status, payload, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": "sometimes"})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown mode status = %d %v", status, payload)
	}
}

// TestHostedClientBodyLogAlwaysRejected proves hosted mode keeps refusing
// detailed body logging, now per client as well as account-wide.
func TestHostedClientBodyLogAlwaysRejected(t *testing.T) {
	t.Parallel()
	_, api, _ := hostedAccountServer(t)
	status, payload, _ := api.request("POST", "/api/admin/client-keys", map[string]any{"name": "hosted client", "description": "body log", "type": "catalogue"})
	if status != http.StatusCreated {
		t.Fatalf("create client key: %d %v", status, payload)
	}
	clientID := payload["id"].(string)

	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": store.BodyLogAlways})
	if status != http.StatusForbidden {
		t.Fatalf("always in hosted mode: %d %v", status, payload)
	}
	if code := payload["error"].(map[string]any)["code"]; code != "hosted_body_logging_disabled" {
		t.Fatalf("error code = %v", code)
	}

	status, payload, _ = api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"log_error_bodies": store.BodyLogNever})
	if status != http.StatusNoContent {
		t.Fatalf("never in hosted mode: %d %v", status, payload)
	}
}
