package server

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSSEKeepaliveWriterEmitsDuringSilence(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newSSEKeepaliveWriter(rec, 20*time.Millisecond)
	defer k.Close()

	time.Sleep(70 * time.Millisecond)
	if !strings.Contains(rec.Body.String(), ": keepalive\n\n") {
		t.Fatalf("expected keepalive frame during silence, got %q", rec.Body.String())
	}

	if _, err := k.Write([]byte("data: x\n\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "data: x") {
		t.Fatalf("write not recorded: %q", rec.Body.String())
	}
}

func TestSSEKeepaliveWriterStopsAfterClose(t *testing.T) {
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
