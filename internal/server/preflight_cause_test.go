package server

import (
	"errors"
	"strings"
	"testing"
)

func TestTruncateReadCause(t *testing.T) {
	if got := truncateReadCause(nil); got != "" {
		t.Fatalf("nil = %q, want empty", got)
	}
	if got := truncateReadCause(errors.New("line one\nline two")); strings.Contains(got, "\n") {
		t.Fatalf("newlines not flattened: %q", got)
	}
	long := strings.Repeat("x", 300)
	got := truncateReadCause(errors.New(long))
	if len(got) > 210 || !strings.HasSuffix(got, "…") {
		t.Fatalf("long cause not truncated: len=%d", len(got))
	}
	if got := truncateReadCause(errUpstreamResponseTooLarge); got == "" {
		t.Fatal("sentinel should produce a cause")
	}
	if got := ctxErrString(nil); got != "" {
		t.Fatalf("nil ctx err = %q, want empty", got)
	}
}
