package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/database"
)

func TestLiveRequestsLifecycleIsolationAndSanitization(t *testing.T) {
	t.Parallel()
	h := &liveHub{subs: map[string]map[chan []byte]struct{}{}}
	a, b := make(chan []byte, 20), make(chan []byte, 20)
	h.subs["a"] = map[chan []byte]struct{}{a: {}}
	h.subs["b"] = map[chan []byte]struct{}{b: {}}
	s := &Server{liveHub: h}
	row := &logRow{accountID: "a", clientKeyID: "client", clientName: "Client", clientRequestID: "request", requestedModel: "route/model", createdAt: time.Now().UTC().Format(time.RFC3339Nano), liveEnabled: true, requestBody: strPtr("sensitive request"), errorBody: strPtr("sensitive response"), errorMessage: strPtr("sensitive detail")}
	s.publishLiveRequest(row, false)
	row.liveFallback = true
	row.attempts = []requestAttempt{{provider: "one", model: "model", result: "skipped", failureClass: "cooldown", errorBody: strPtr("sensitive attempt")}}
	row.liveAttempt = &requestAttempt{provider: "two", model: "model"}
	s.publishLiveRequest(row, false)
	rows := h.requestSnapshot("a", true)
	if len(rows) != 1 || !rows[0].FallbackUsed || len(rows[0].Attempts) != 2 || rows[0].Attempts[1].Result != "running" {
		t.Fatalf("unexpected live state: %+v", rows)
	}
	if len(h.requestSnapshot("b", false)) != 0 || len(b) != 0 {
		t.Fatal("request crossed account boundary")
	}
	row.httpStatus = 200
	row.resolvedProvider, row.resolvedModel = strPtr("two"), strPtr("model")
	row.inputTokens = new(int64(7))
	s.publishLiveRequest(row, true)
	if len(h.requestSnapshot("a", true)) != 0 {
		t.Fatal("finished request remained active")
	}
	rows = h.requestSnapshot("a", false)
	if rows[0].State != "success" || rows[0].CompletedAt == "" || *rows[0].InputTokens != 7 {
		t.Fatal("completion did not fill in row")
	}
	for len(a) > 0 {
		if strings.Contains(string(<-a), "sensitive") {
			t.Fatal("body/error content escaped into live SSE")
		}
	}
	row.liveEnabled = false
	row.clientRequestID = "disabled"
	s.publishLiveRequest(row, false)
	if len(h.requestSnapshot("a", false)) != 1 {
		t.Fatal("logging-disabled request was published")
	}
}

func TestLiveRequestsRetentionAndCompletedCap(t *testing.T) {
	t.Parallel()
	h := &liveHub{}
	now := time.Now().Truncate(time.Second)
	for i := 0; i < 205; i++ {
		h.putRequest("a", liveRequestView{globalActivityView: globalActivityView{activityView: activityView{ID: fmt.Sprint(i)}}, CompletedAt: now.Add(time.Duration(i) * time.Microsecond).Format(time.RFC3339Nano)})
	}
	h.putRequest("a", liveRequestView{globalActivityView: globalActivityView{activityView: activityView{ID: "active"}}})
	if got := len(h.requestSnapshot("a", false)); got != 201 {
		t.Fatalf("got %d rows, want 200 completed + active", got)
	}
	for _, row := range h.requestSnapshot("a", false) {
		if row.ID == "0" {
			t.Fatal("cap retained the oldest completion (mixed timestamp precision)")
		}
	}
	h.mu.Lock()
	h.pruneRequestsLocked(now.Add(31 * time.Second))
	h.mu.Unlock()
	if got := len(h.requestSnapshot("a", false)); got != 1 {
		t.Fatalf("got %d rows after expiry, want active only", got)
	}
}

func TestLiveRequestsHTTPStartCompleteAndRecovery(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	upstream := mockUpstream(t)
	api, _, _, secret := loggingTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			close(started)
			<-release
		}
		upstream(w, r)
	})
	// Always release the upstream if an assertion fails.
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	result := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest("POST", api.base+"/v1/chat/completions", strings.NewReader(`{"model":"provider-a/model-a","messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	status, payload, _ := api.request("GET", "/api/admin/live/requests", nil)
	if status != 200 || len(payload["data"].([]any)) != 1 {
		t.Fatalf("live seed: %d", status)
	}
	id := payload["data"].([]any)[0].(map[string]any)["id"].(string)
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	// Force recovery outside the in-memory retention window.
	api.server.liveHub.withdrawRequest(database.LocalAccountID, id)
	status, payload, _ = api.request("POST", "/api/admin/live/requests/reconcile", map[string]any{"ids": []string{id, "unknown"}})
	if status != 200 || len(payload["data"].([]any)) != 1 || len(payload["missing"].([]any)) != 1 {
		t.Fatalf("recovery: %d %+v", status, payload)
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), "provider-secret") {
		t.Fatal("credential exposed")
	}
	row := payload["data"].([]any)[0].(map[string]any)
	if row["state"] != "success" || row["completed_at"] == "" || row["input_tokens"] != float64(10) {
		t.Fatal("stored completion not recovered")
	}
	resp, err := http.Get(api.base + "/api/admin/live/requests")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated endpoint returned %d", resp.StatusCode)
	}
}

func TestLiveRequestsLoggingDisabled(t *testing.T) {
	t.Parallel()
	api, _, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	ch := make(chan []byte, 20)
	api.server.liveHub.mu.Lock()
	api.server.liveHub.subs = map[string]map[chan []byte]struct{}{database.LocalAccountID: {ch: {}}}
	api.server.liveHub.mu.Unlock()
	status, _, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"logging_enabled": false})
	if status != 204 {
		t.Fatalf("logging switch: %d", status)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{}})
	if resp.StatusCode != 200 {
		t.Fatalf("request: %d", resp.StatusCode)
	}
	if len(api.server.liveHub.requestSnapshot(database.LocalAccountID, false)) != 0 {
		t.Fatal("logging-disabled request reached the live table")
	}
	if len(ch) != 0 {
		t.Fatal("logging-disabled request emitted request-level metadata")
	}
}
