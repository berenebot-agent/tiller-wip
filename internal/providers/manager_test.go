package providers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestSafeRefreshErrorDoesNotExposeRequestDetails(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "deadline", err: context.DeadlineExceeded, want: "Provider discovery timed out."},
		{name: "cancelled", err: context.Canceled, want: "Provider discovery was cancelled."},
		{name: "http status", err: errors.New("model discovery returned HTTP 401"), want: "Provider discovery returned HTTP 401."},
		{name: "decode", err: errors.New(`decode model catalogue: invalid character 's' looking for beginning of value`), want: "Provider discovery failed."},
		{name: "transport URL", err: &url.Error{Op: "Get", URL: "https://example.test/models?api_key=secret", Err: fmt.Errorf("dial failed")}, want: "Provider discovery request failed."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeRefreshError(tt.err); got != tt.want {
				t.Fatalf("safeRefreshError() = %q, want %q", got, tt.want)
			}
		})
	}
}
