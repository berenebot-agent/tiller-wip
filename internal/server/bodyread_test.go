package server

import (
	"errors"
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
	g := &bodyReadGate{}
	held := 0
	for i := 0; i < maxConcurrentBodyReads; i++ {
		if !g.acquire() {
			t.Fatalf("gate refused at %d, before the cap", i)
		}
		held++
	}
	if g.acquire() {
		t.Fatal("gate admitted beyond the cap")
	}
	for i := 0; i < held; i++ {
		g.release()
	}
	if g.active.Load() != 0 {
		t.Fatalf("active = %d after releasing all, want 0", g.active.Load())
	}
	if !g.acquire() {
		t.Fatal("released capacity was not reusable")
	}
	g.release()
}

// TestBodyReadGateConcurrentAcquireRelease exercises the counter under
// contention so a race is caught by the race detector.
func TestBodyReadGateConcurrentAcquireRelease(t *testing.T) {
	g := &bodyReadGate{}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g.acquire() {
				g.release()
			}
		}()
	}
	wg.Wait()
	if g.active.Load() != 0 {
		t.Fatalf("active = %d after concurrent use, want 0", g.active.Load())
	}
}

// TestWithBodyReadDeadlineRunsFunction proves the helper runs the function and
// returns its error, including on a writer that does not support deadlines
// (httptest.NewRecorder), where the deadline is a graceful no-op.
func TestWithBodyReadDeadlineRunsFunction(t *testing.T) {
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
