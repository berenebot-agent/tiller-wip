package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEKeepaliveWriterEmitsDuringSilence(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	k := newSSEKeepaliveWriter(rec, 20*time.Millisecond)
	defer k.Close()

	// Read the recorder under the writer's mutex: the keepalive goroutine writes
	// through it, and httptest.ResponseRecorder is not safe for concurrent use.
	body := func() string {
		k.mu.Lock()
		defer k.mu.Unlock()
		return rec.Body.String()
	}

	// Poll rather than sleep a fixed amount: a loaded machine (many tests in
	// parallel) can delay the first keepalive well past a fixed sleep.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(body(), ": keepalive\n\n") {
		if time.Now().After(deadline) {
			t.Fatalf("expected keepalive frame during silence, got %q", body())
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := k.Write([]byte("data: x\n\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body(), "data: x") {
		t.Fatalf("write not recorded: %q", body())
	}
}

func TestSSEKeepaliveWriterStopsAfterClose(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	k := newSSEKeepaliveWriter(rec, 20*time.Millisecond)
	k.Close()

	time.Sleep(60 * time.Millisecond)
	if strings.Contains(rec.Body.String(), ": keepalive") {
		t.Fatalf("keepalive emitted after Close: %q", rec.Body.String())
	}
	if _, err := k.Write([]byte("data: y\n\n")); err != io.ErrClosedPipe {
		t.Fatalf("write after Close error = %v, want io.ErrClosedPipe", err)
	}
}
