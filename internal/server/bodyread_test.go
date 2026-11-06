package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBodyReadGateBoundsConcurrency proves the gate refuses once the cap is
// reached and admits again after release, so a slow-upload flood cannot park an
// unbounded number of body buffers.
func TestBodyReadGateBoundsConcurrency(t *testing.T) {
	t.Parallel()
	g := &bodyReadGate{}
	held := 0
	for i := 0; i < maxConcurrentBodyReads; i++ {
		if !g.acquire("") {
			t.Fatalf("gate refused at %d, before the cap", i)
		}
		held++
	}
	if g.acquire("") {
		t.Fatal("gate admitted beyond the cap")
	}
	for i := 0; i < held; i++ {
		g.release("")
	}
	if n := g.inUse(); n != 0 {
		t.Fatalf("active = %d after releasing all, want 0", n)
	}
	if !g.acquire("") {
		t.Fatal("released capacity was not reusable")
	}
	g.release("")
}

// TestBodyReadGatePerAccountShare proves one account saturating its share of
// the inference gate cannot deny a second account (the pentest's cross-tenant
// denial of inference), and that the aggregate ceiling still holds across
// accounts.
func TestBodyReadGatePerAccountShare(t *testing.T) {
	t.Parallel()
	g := &bodyReadGate{max: maxConcurrentBodyReads, perKey: maxConcurrentBodyReadsPerAccount}
	for i := 0; i < maxConcurrentBodyReadsPerAccount; i++ {
		if !g.acquire("acct-a") {
			t.Fatalf("account A refused at %d, before its share", i)
		}
	}
	if g.acquire("acct-a") {
		t.Fatal("account A admitted beyond its per-account share")
	}
	if !g.acquire("acct-b") {
		t.Fatal("account B was denied while only account A was saturated")
	}
	g.release("acct-b")
	// Fill the aggregate with distinct accounts; the global cap still applies.
	for i := maxConcurrentBodyReadsPerAccount; i < maxConcurrentBodyReads; i++ {
		if !g.acquire(fmt.Sprintf("acct-%d", i)) {
			t.Fatalf("aggregate refused at %d, before the cap", i)
		}
	}
	if g.acquire("acct-new") {
		t.Fatal("gate admitted beyond the aggregate cap")
	}
	g.release("acct-a")
	if g.acquire("acct-a") == false {
		t.Fatal("a released slot was not reusable by its account")
	}
	g.mu.Lock()
	keys := len(g.byKey)
	g.mu.Unlock()
	if keys != maxConcurrentBodyReads-maxConcurrentBodyReadsPerAccount+1 {
		t.Fatalf("tracked accounts = %d", keys)
	}
}

// TestBodyReadGateReleaseForgetsIdleAccounts keeps the per-account map bounded
// by the accounts currently mid-body.
func TestBodyReadGateReleaseForgetsIdleAccounts(t *testing.T) {
	t.Parallel()
	g := &bodyReadGate{max: maxConcurrentBodyReads, perKey: maxConcurrentBodyReadsPerAccount}
	if !g.acquire("acct-a") {
		t.Fatal("refused")
	}
	g.release("acct-a")
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.byKey) != 0 || g.active != 0 {
		t.Fatalf("byKey=%v active=%d after release", g.byKey, g.active)
	}
}

// TestPreAuthBodyGateIsSeparateFromInference proves a saturated pre-auth gate
// (anonymous passkey-finish bodies) leaves the inference gate untouched.
func TestPreAuthBodyGateIsSeparateFromInference(t *testing.T) {
	t.Parallel()
	_, _, s := newManagementHarness(t)
	for i := 0; i < maxConcurrentPreAuthBodyReads; i++ {
		if !s.preAuthBodyReads.acquire("") {
			t.Fatalf("pre-auth gate refused at %d", i)
		}
	}
	if s.preAuthBodyReads.acquire("") {
		t.Fatal("pre-auth gate admitted beyond its cap")
	}
	if !s.bodyReads.acquire("acct") {
		t.Fatal("inference gate denied while only the pre-auth gate was full")
	}
	s.bodyReads.release("acct")
	for i := 0; i < maxConcurrentPreAuthBodyReads; i++ {
		s.preAuthBodyReads.release("")
	}
}

// TestPasskeyFinishBodyUsesPreAuthGate proves the anonymous passkey-finish body
// read charges the pre-auth gate, not the inference gate.
func TestPasskeyFinishBodyUsesPreAuthGate(t *testing.T) {
	t.Parallel()
	_, _, s := newManagementHarness(t)
	for i := 0; i < maxConcurrentPreAuthBodyReads; i++ {
		s.preAuthBodyReads.acquire("")
	}
	defer func() {
		for i := 0; i < maxConcurrentPreAuthBodyReads; i++ {
			s.preAuthBodyReads.release("")
		}
	}()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/passkey/finish", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if s.passkeyFinishBody(rec, req) {
		t.Fatal("passkey finish admitted with the pre-auth gate full")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if n := s.bodyReads.inUse(); n != 0 {
		t.Fatalf("inference gate slots in use = %d, want 0", n)
	}
}

// TestBodyReadGateConcurrentAcquireRelease exercises the counter under
// contention so a race is caught by the race detector.
func TestBodyReadGateConcurrentAcquireRelease(t *testing.T) {
	t.Parallel()
	g := &bodyReadGate{max: maxConcurrentBodyReads, perKey: maxConcurrentBodyReadsPerAccount}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("acct-%d", i%5)
			if g.acquire(key) {
				g.release(key)
			}
		}(i)
	}
	wg.Wait()
	if n := g.inUse(); n != 0 {
		t.Fatalf("active = %d after concurrent use, want 0", n)
	}
}

// TestWithBodyReadDeadlineRunsFunction proves the helper runs the function and
// returns its error, including on a writer that does not support deadlines
// (httptest.NewRecorder), where the deadline is a graceful no-op.
func TestWithBodyReadDeadlineRunsFunction(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	ran := false
	err := withBodyReadDeadline(rec, func() error {
		ran = true
		return nil
	})
	if !ran || err != nil {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
}

// TestDecodeJSONBodyRejectsStalledUpload proves a peer that sends a partial
// body and then stalls is released by the read deadline instead of holding the
// handler forever. The deadline is shortened for the test; the wire behaviour
// (SetReadDeadline through http.ResponseController) is the same.
//
// Deliberately NOT parallel: it shortens the package-level bodyReadDeadline,
// which the other decode tests read.
func TestDecodeJSONBodyRejectsStalledUpload(t *testing.T) {
	old := bodyReadDeadline
	bodyReadDeadline = 50 * time.Millisecond
	defer func() { bodyReadDeadline = old }()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var target map[string]any
		err := decodeJSONBody(w, r, &target, 8<<10, true)
		if !errors.Is(err, errBodyReadTimeout) {
			t.Errorf("decode err = %v, want errBodyReadTimeout", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(`{"email":`))
		// Never close: the peer has stalled mid-body.
	}()
	req, err := http.NewRequest(http.MethodPost, server.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	_ = pw.Close()
}

// TestDecodeJSONBodyAcceptsNormalBody guards against the deadline wiring
// breaking the ordinary path.
func TestDecodeJSONBodyAcceptsNormalBody(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":"a@b.test","password":"correct horse battery"}`))
	req.Header.Set("Content-Type", "application/json")
	var target struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(rec, req, &target, 8<<10, true); err != nil {
		t.Fatalf("normal body rejected: %v", err)
	}
	if target.Email != "a@b.test" {
		t.Fatalf("email = %q", target.Email)
	}
}

// TestDecodeJSONBodyReportsMalformedJSON proves malformed JSON is still reported
// as such and not confused with a timeout.
func TestDecodeJSONBodyReportsMalformedJSON(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"email":`))
	req.Header.Set("Content-Type", "application/json")
	var target struct{}
	err := decodeJSONBody(rec, req, &target, 8<<10, true)
	if err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	if err == errBodyReadTimeout {
		t.Fatal("malformed JSON reported as a read timeout")
	}
}

// TestIsTimeoutError keeps the timeout classifier honest for the error shapes
// the deadline can surface.
func TestIsTimeoutError(t *testing.T) {
	t.Parallel()
	if !isTimeoutError(errBodyReadTimeout) {
		t.Fatal("errBodyReadTimeout not classified as a timeout")
	}
	if isTimeoutError(io.EOF) {
		t.Fatal("EOF classified as a timeout")
	}
	if isTimeoutError(nil) {
		t.Fatal("nil classified as a timeout")
	}
	_ = time.Second
}

// TestBodyReadGateReleasedBeforeStreamEnds proves the gate covers only the body
// read: while an upstream stream is still open the slot must already be free,
// or 64 long streams would block /v1 for every tenant.
func TestBodyReadGateReleasedBeforeStreamEnds(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	api, _, _, secret := loggingTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"model-a","object":"model"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		once.Do(func() { close(reached) })
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t.Cleanup(func() { close(hold) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, "POST", api.base+"/v1/chat/completions", strings.NewReader(`{"model":"provider-a/model-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+secret)
		if resp, err := api.client.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("upstream stream was never reached")
	}
	if n := api.server.bodyReads.inUse(); n != 0 {
		t.Fatalf("body-read slots held during an open stream = %d, want 0", n)
	}
	cancel()
	<-done
}
