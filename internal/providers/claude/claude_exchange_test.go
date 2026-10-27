package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tiller-router/tiller-router/internal/providers/oauth"
)

func TestExchangeSendsState(t *testing.T) {
	t.Parallel()
	var got map[string]string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":3600}`))
	}))
	defer upstream.Close()

	client := &http.Client{Transport: &routingTransport{server: upstream}, Timeout: 5 * time.Second}
	if _, err := Exchange(context.Background(), client, "the-code", ManualRedirectURI, "verifier", "the-state"); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if got["state"] != "the-state" || got["code"] != "the-code" || got["redirect_uri"] != ManualRedirectURI {
		t.Fatalf("token body = %v", got)
	}
}

func TestParseCallbackBareCodeAndState(t *testing.T) {
	t.Parallel()
	cb, err := ParseCallback("  abc123#st4te \n")
	if err != nil || cb.Code != "abc123" || cb.State != "st4te" {
		t.Fatalf("bare code#state = %+v, %v", cb, err)
	}
	for _, bad := range []string{"", "abc123", "#state", "code#", "two words#state"} {
		if _, err := ParseCallback(bad); err != oauth.ErrCallback {
			t.Fatalf("ParseCallback(%q) err = %v, want ErrCallback", bad, err)
		}
	}
	cb, err = ParseCallback("https://platform.claude.com/oauth/code/callback?code=c&state=s")
	if err != nil || cb.Code != "c" || cb.State != "s" {
		t.Fatalf("full URL = %+v, %v", cb, err)
	}
}
