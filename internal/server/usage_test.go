package server

import (
	"fmt"
	"io"
	"testing"
	"time"
)

func TestUsageEndpointPerWindowTotals(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	// Create a real virtual group + virtual model so the virtual request's
	// requested_model matches a real canonical ID (virtual/coding).
	var providerID, modelID string
	if err := db.SQL.QueryRow(`SELECT id FROM providers WHERE name='provider-a'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	status, payload, _ := api.request("POST", "/api/admin/virtual-groups", map[string]any{"name": "virtual"})
	if status != 201 {
		t.Fatalf("create virtual group: %d %v", status, payload)
	}
	groupID := payload["id"].(string)
	status, payload, _ = api.request("POST", "/api/admin/virtual-models", map[string]any{"group_id": groupID, "name": "coding", "target_provider_id": providerID, "target_model_id": modelID})
	if status != 201 {
		t.Fatalf("create virtual model: %d %v", status, payload)
	}
	now := time.Now().UTC()
	// Insert rows at controlled times with known token totals.
	// Direct-real rows (requested_model = provider-a/model-a):
	// - 30 min ago: 100k tokens  -> in 1h, 24h, 7d
	// - 2 hours ago: 200k tokens -> in 24h, 7d (not 1h)
	// - 3 days ago: 300k tokens  -> in 7d (not 1h/24h)
	// - 10 days ago: 400k tokens -> in none
	// Virtual rows (requested_model = virtual/coding, resolved to provider-a/model-a):
	// - 30 min ago: 50k tokens  -> in 1h, 24h, 7d
	// - 2 hours ago: 100k tokens -> in 24h, 7d (not 1h)
	rows := []struct {
		ago   time.Duration
		total int64
		kind  string // "direct" or "virtual"
	}{
		{30 * time.Minute, 100000, "direct"},
		{2 * time.Hour, 200000, "direct"},
		{3 * 24 * time.Hour, 300000, "direct"},
		{10 * 24 * time.Hour, 400000, "direct"},
		{30 * time.Minute, 50000, "virtual"},
		{2 * time.Hour, 100000, "virtual"},
	}
	for i, r := range rows {
		created := now.Add(-r.ago).Format(time.RFC3339Nano)
		in := r.total / 2
		out := r.total - in
		requested := "provider-a/model-a"
		if r.kind == "virtual" {
			requested = "virtual/coding"
		}
		if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("row%d", i), clientID, requested, "provider-a", "model-a", "chat", 0, 200, 1, in, out, "req-x", created); err != nil {
			t.Fatal(err)
		}
	}
	status, payload, _ = api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	// Client totals include both direct and virtual traffic.
	ck := payload["client_keys"].(map[string]any)[clientID].(map[string]any)
	if ck["1h"] != float64(150000) {
		t.Fatalf("1h client usage wrong: %v", ck)
	}
	if ck["24h"] != float64(450000) {
		t.Fatalf("24h client usage wrong: %v", ck)
	}
	if ck["7d"] != float64(750000) {
		t.Fatalf("7d client usage wrong: %v", ck)
	}
	// The direct real ID must be absent from virtual_models.
	vm := payload["virtual_models"].(map[string]any)
	if _, ok := vm["provider-a/model-a"]; ok {
		t.Fatalf("direct real ID misclassified as virtual: %v", vm)
	}
	// The virtual ID has only the virtual request's totals.
	v := vm["virtual/coding"].(map[string]any)
	if v["1h"] != float64(50000) || v["24h"] != float64(150000) || v["7d"] != float64(150000) {
		t.Fatalf("virtual usage wrong: %v", v)
	}
	// The resolved real target includes both direct and virtual traffic.
	rm := payload["real_models"].(map[string]any)["provider-a/model-a"].(map[string]any)
	if rm["1h"] != float64(150000) || rm["24h"] != float64(450000) || rm["7d"] != float64(750000) {
		t.Fatalf("real usage wrong: %v", rm)
	}
}

func TestUsageEndpointEmpty(t *testing.T) {
	api, _, _, _ := loggingTestHarness(t, mockUpstream(t))
	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	if len(payload["client_keys"].(map[string]any)) != 0 || len(payload["virtual_models"].(map[string]any)) != 0 || len(payload["real_models"].(map[string]any)) != 0 {
		t.Fatalf("expected empty usage, got %v", payload)
	}
}

// TestUsageTargetLastOutcome verifies that a routed request records its outcome
// in the in-memory target_last_outcome map and that it is surfaced by the usage
// endpoint keyed by provider model ID.
func TestUsageTargetLastOutcome(t *testing.T) {
	api, db, _, secret := loggingTestHarness(t, mockUpstream(t))

	// No requests yet: usage should expose an empty (or absent) map.
	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	if v, ok := payload["target_last_outcome"]; ok {
		if m := v.(map[string]any); len(m) != 0 {
			t.Fatalf("expected empty target_last_outcome before traffic, got %v", m)
		}
	}

	// Drive a successful request through provider-a/model-a.
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}

	status, payload, _ = api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	var modelID string
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	last, ok := payload["target_last_outcome"].(map[string]any)[modelID].(map[string]any)
	if !ok {
		t.Fatalf("%s missing from target_last_outcome: %v", modelID, payload["target_last_outcome"])
	}
	if last["status"] != float64(200) {
		t.Fatalf("last status = %v, want 200", last["status"])
	}
	if last["is_success"] != true {
		t.Fatalf("last is_success = %v, want true", last["is_success"])
	}
	if at, _ := last["at"].(string); at == "" {
		t.Fatal("expected a last_outcome timestamp")
	}
}

// TestRecordLastOutcomeNoAttempts verifies that a log row with no attempted
// targets (e.g. model-not-found, translation error, client cancellation) does
// not touch the in-memory outcome map.
func TestRecordLastOutcomeNoAttempts(t *testing.T) {
	s := &Server{}
	row := &logRow{httpStatus: 404, errorText: strPtr("model_not_found")}
	s.recordLastOutcome(row)
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	if len(s.lastOutcome) != 0 {
		t.Fatalf("recordLastOutcome wrote an outcome without any attempted target: %v", s.lastOutcome)
	}
}

// TestRecordLastOutcomeFailedAttempt records a red outcome for the target that
// was actually attempted and failed, even though no resolved target was set on
// the row. This is the case where the original implementation silently dropped
// the failure and left the dot grey.
func TestRecordLastOutcomeFailedAttempt(t *testing.T) {
	s := &Server{}
	row := &logRow{
		httpStatus: 502,
		createdAt:  "2026-09-02T12:00:00Z",
		attempts: []requestAttempt{
			{providerModelID: "pm-failed", provider: "main", model: "argus-5.3-codex-spark", result: "failed", httpStatus: 502, failureClass: "upstream_unreachable"},
		},
	}
	s.recordLastOutcome(row)
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	out, ok := s.lastOutcome["pm-failed"]
	if !ok {
		t.Fatalf("expected failed target to be recorded: %v", s.lastOutcome)
	}
	if out.At == "" {
		t.Fatal("expected a completion timestamp")
	}
	if out.Status != 502 {
		t.Fatalf("status = %d, want 502", out.Status)
	}
	if out.IsSuccess {
		t.Fatal("expected is_success=false for a failed attempt")
	}
}

// TestRecordLastOutcomeSkippedAttempt verifies that a target that was skipped
// (never actually called — unavailable or protocol mismatch) does not force a
// red dot.
func TestRecordLastOutcomeSkippedAttempt(t *testing.T) {
	s := &Server{}
	row := &logRow{
		httpStatus: 503,
		createdAt:  "2026-09-02T12:00:00Z",
		attempts: []requestAttempt{
			{providerModelID: "pm-skipped", provider: "main", model: "argus-5.3-codex-spark", result: "skipped", failureClass: "unavailable"},
		},
	}
	s.recordLastOutcome(row)
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	if _, ok := s.lastOutcome["pm-skipped"]; ok {
		t.Fatalf("skipped target should not be recorded: %v", s.lastOutcome)
	}
}

// TestRecordLastOutcomeOrderedFallback records per-target outcomes across an
// ordered fallback chain: the failed target turns red, the succeeding target
// turns green, each reflecting its own last outcome.
func TestRecordLastOutcomeOrderedFallback(t *testing.T) {
	s := &Server{}
	row := &logRow{
		httpStatus: 200,
		createdAt:  "2026-09-02T12:00:00Z",
		attempts: []requestAttempt{
			{providerModelID: "pm-primary", provider: "main", model: "argus-5.3-codex-spark", result: "failed", httpStatus: 503, failureClass: "http_503"},
			{providerModelID: "pm-backup", provider: "main", model: "backup-model", result: "success", httpStatus: 200},
		},
	}
	s.recordLastOutcome(row)
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	failed, ok := s.lastOutcome["pm-primary"]
	if !ok || failed.IsSuccess {
		t.Fatalf("expected failed target red: %v", s.lastOutcome)
	}
	success, ok := s.lastOutcome["pm-backup"]
	if !ok || !success.IsSuccess {
		t.Fatalf("expected succeeding target green: %v", s.lastOutcome)
	}
}

func TestRecordLastOutcomeRunsWhenLoggingDisabled(t *testing.T) {
	api, db, clientID, secret := loggingTestHarness(t, mockUpstream(t))
	status, _, _ := api.request("PATCH", "/api/admin/client-keys/"+clientID, map[string]any{"logging_enabled": false})
	if status != 204 {
		t.Fatalf("disable logging: %d", status)
	}
	resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{"model": "provider-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "hello"}}})
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("request status %d", resp.StatusCode)
	}
	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	var modelID string
	if err := db.SQL.QueryRow(`SELECT id FROM provider_models WHERE upstream_model_id='model-a'`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	last := payload["target_last_outcome"].(map[string]any)[modelID].(map[string]any)
	if last["status"] != float64(200) || last["is_success"] != true {
		t.Fatalf("logging-disabled outcome wrong: %v", last)
	}
}

func TestRecordLastOutcomeDoesNotCollideForSameUpstreamNames(t *testing.T) {
	s := &Server{}
	s.recordLastOutcome(&logRow{attempts: []requestAttempt{
		{providerModelID: "pm-one", provider: "same", model: "same", result: "failed", httpStatus: 500},
		{providerModelID: "pm-two", provider: "same", model: "same", result: "success", httpStatus: 200},
	}})
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	if len(s.lastOutcome) != 2 || s.lastOutcome["pm-one"].IsSuccess || !s.lastOutcome["pm-two"].IsSuccess {
		t.Fatalf("outcomes were not keyed by provider model ID: %+v", s.lastOutcome)
	}
}

func TestRecordLastOutcomeNetworkFailureFollowedByFallbackSuccess(t *testing.T) {
	s := &Server{}
	row := &logRow{httpStatus: 200, createdAt: "2026-09-02T12:00:00Z", attempts: []requestAttempt{
		{providerModelID: "pm-primary", provider: "primary", model: "model-a", result: "failed", httpStatus: 0, failureClass: "upstream_unreachable"},
		{providerModelID: "pm-backup", provider: "backup", model: "model-b", result: "success", httpStatus: 200},
	}}
	s.recordLastOutcome(row)
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	if got := s.lastOutcome["pm-primary"]; got.Status != 0 || got.IsSuccess {
		t.Fatalf("network failure outcome = %+v, want status 0 and failure", got)
	}
	if got := s.lastOutcome["pm-backup"]; got.Status != 200 || !got.IsSuccess {
		t.Fatalf("fallback success outcome = %+v", got)
	}
	if got := s.lastOutcome["pm-backup"]; got.At == row.createdAt {
		t.Fatalf("outcome timestamp used request start: %+v", got)
	}
}

func TestRecordLastOutcomeUsesLaterRecordingTime(t *testing.T) {
	s := &Server{}
	first := &logRow{createdAt: "2026-09-02T12:00:00Z", attempts: []requestAttempt{{providerModelID: "pm-model", provider: "provider", model: "model", result: "failed"}}}
	s.recordLastOutcome(first)
	s.lastOutcomeMu.RLock()
	firstRecordedAt := s.lastOutcome["pm-model"].At
	s.lastOutcomeMu.RUnlock()

	time.Sleep(time.Millisecond)
	second := &logRow{createdAt: "2026-09-02T11:00:00Z", attempts: []requestAttempt{{providerModelID: "pm-model", provider: "provider", model: "model", result: "success", httpStatus: 200}}}
	s.recordLastOutcome(second)

	s.lastOutcomeMu.RLock()
	got := s.lastOutcome["pm-model"]
	s.lastOutcomeMu.RUnlock()
	firstAt, err := time.Parse(time.RFC3339Nano, firstRecordedAt)
	if err != nil {
		t.Fatal(err)
	}
	gotAt, err := time.Parse(time.RFC3339Nano, got.At)
	if err != nil {
		t.Fatal(err)
	}
	if !gotAt.After(firstAt) {
		t.Fatalf("later outcome timestamp did not advance: first=%s got=%s", firstRecordedAt, got.At)
	}
	if got.Status != 200 || !got.IsSuccess {
		t.Fatalf("later outcome did not win: %+v", got)
	}
}

func TestUsageCacheHitWindows(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	now := time.Now().UTC()
	type row struct {
		in, out, cacheRead int64
		hasCache           bool
	}
	// Three real-model groups resolved to the same target:
	//  - cache-valid: cache_read present (e.g. Anthropic reporting cache).
	//  - zero-cache:  cache_read present but 0 (reported cache, no hits).
	//  - no-cache:    cache_read absent (e.g. Ollama — not reported).
	rows := []row{
		{in: 1000, out: 100, cacheRead: 900, hasCache: true}, // 90% hit
		{in: 1000, out: 100, cacheRead: 500, hasCache: true}, // 50% hit
		{in: 1000, out: 100, cacheRead: 0, hasCache: true},   // 0% hit (real)
		{in: 1000, out: 100, cacheRead: 0, hasCache: false},  // missing -> n.a.
	}
	for i, r := range rows {
		created := now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
		resolvedModel := "model-a"
		req := "provider-a/model-a"
		var cacheRead any
		if !r.hasCache {
			req = "provider-a/model-b"
			resolvedModel = "model-b"
		} else {
			cacheRead = r.cacheRead
		}
		if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,input_tokens,output_tokens,cache_read_input_tokens,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("cache%d", i), clientID, req, "provider-a", resolvedModel, "chat", 0, 200, 1, r.in, r.out, cacheRead, "req-c", created); err != nil {
			t.Fatal(err)
		}
	}
	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	// Mixed window: cache-valid rows (90%, 50%, 0%) blend to
	// (900+500+0)/(1000+1000+1000) = 1400/3000 ≈ 46.67%. n.a. rows are dropped.
	if v, ok := payload["real_cache"].(map[string]any)["provider-a/model-a"].(map[string]any); !ok {
		t.Fatalf("expected real_cache for provider-a/model-a, got %v", payload["real_cache"])
	} else if v["1h"] == nil {
		t.Fatalf("expected non-nil cache %% for blended window, got %v", v)
	} else if p := v["1h"].(float64); p < 46.4 || p > 47.0 {
		t.Fatalf("blended cache %% wrong: %.3f (want ~46.67)", p)
	}
	// n.a. window: only non-reporting rows -> null.
	if v, ok := payload["real_cache"].(map[string]any)["provider-a/model-b"].(map[string]any); !ok {
		t.Fatalf("expected real_cache for provider-a/model-b, got %v", payload["real_cache"])
	} else if v["1h"] != nil {
		t.Fatalf("expected null (n.a.) cache %% for non-reporting window, got %v", v)
	}
}

// TestUsageTargetHealthIncludesFailedAttempts verifies that target_health
// surfaces targets that were attempted (even when the fallback chain
// ultimately exhausted and the request_logs row has NULL resolved_provider/
// resolved_model). Without the request_attempts UNION these targets would
// silently disappear and the admin UI would show them as untried neutral
// instead of broken red.
func TestUsageTargetHealthIncludesFailedAttempts(t *testing.T) {
	api, db, clientID, _ := loggingTestHarness(t, mockUpstream(t))
	now := time.Now().UTC()

	// Virtual model whose fallback chain exhausted: both targets failed,
	// no resolved_provider/resolved_model on the parent row.
	if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,attempt_count,fallback_used,fallback_reason,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"exhausted", clientID, "virtual/coding", "virtual/coding", "virtual", "v1", "virtual/coding", nil, nil, "chat", 0, 503, 10, 2, 1, "upstream_error", "req-exhausted", now.Add(-5*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// Failed attempt on target A (provider-a/model-a, 401).
	if _, err := db.SQL.Exec(`INSERT INTO request_attempts(id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"a-failed", "exhausted", 1, "provider-a", "model-a", "failed", 401, "http_401", 5, now.Add(-5*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// Skipped attempt on target B (provider-a/model-b, disabled).
	if _, err := db.SQL.Exec(`INSERT INTO request_attempts(id,request_log_id,attempt_number,provider,model,result,http_status,failure_class,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"b-skipped", "exhausted", 2, "provider-a", "model-b", "skipped", nil, "unavailable", 0, now.Add(-5*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	// Successful run on a different target (provider-b/model-c) so we have a
	// confirmed-success entry as well, sanity-checking that the union does
	// not double-count.
	if _, err := db.SQL.Exec(`INSERT INTO request_logs(id,client_key_id,requested_model,exposed_model,route_kind,route_model_id,route_model,resolved_provider,resolved_model,protocol,streaming,http_status,latency_ms,client_request_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"happy", clientID, "provider-b/model-c", "provider-b/model-c", "real", "rb1", "provider-b/model-c", "provider-b", "model-c", "chat", 0, 200, 7, "req-happy", now.Add(-2*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO request_attempts(id,request_log_id,attempt_number,provider,model,result,http_status,latency_ms,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		"c-success", "happy", 1, "provider-b", "model-c", "success", 200, 7, now.Add(-2*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	status, payload, _ := api.request("GET", "/api/admin/usage", nil)
	if status != 200 {
		t.Fatalf("usage: %d %v", status, payload)
	}
	health := payload["target_health"].(map[string]any)

	// Failed target: success_24h=false, failure_1h=true.
	if a, ok := health["provider-a/model-a"].(map[string]any); !ok {
		t.Fatalf("expected failed target in target_health, got %v", health)
	} else if a["success_24h"] != false || a["failure_1h"] != true || a["success_1h"] != false {
		t.Fatalf("failed target health wrong: %+v", a)
	}

	// Skipped target: surfaces as failure too — it was attempted, the
	// router declined to call it, that is a health signal.
	if b, ok := health["provider-a/model-b"].(map[string]any); !ok {
		t.Fatalf("expected skipped target in target_health, got %v", health)
	} else if b["success_24h"] != false || b["failure_1h"] != true {
		t.Fatalf("skipped target health wrong: %+v", b)
	}

	// Successful target: success across the board.
	if c, ok := health["provider-b/model-c"].(map[string]any); !ok {
		t.Fatalf("expected success target in target_health, got %v", health)
	} else if c["success_24h"] != true || c["failure_1h"] != false || c["success_1h"] != true {
		t.Fatalf("success target health wrong: %+v", c)
	}
}
