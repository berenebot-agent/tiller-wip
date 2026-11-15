package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tiller-router/tiller-router/internal/hostedauth"
	"github.com/tiller-router/tiller-router/internal/store"
)

type fakeTurnstile struct{}

// RoundTrip approves every challenge and echoes the submitted token as the
// action, so a test passes captcha_token "signup" for the signup flow.
func (fakeTurnstile) RoundTrip(r *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(r.Body)
	token := ""
	for _, kv := range strings.Split(string(body), "&") {
		if strings.HasPrefix(kv, "response=") {
			token = strings.TrimPrefix(kv, "response=")
		}
	}
	out, _ := json.Marshal(map[string]any{"success": true, "hostname": "tiller.example.com", "action": token})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(out))), Header: http.Header{}}, nil
}

// enableTestCaptcha turns Turnstile on with a verifier that approves any token.
func enableTestCaptcha(t *testing.T, app *Server) {
	t.Helper()
	ctx := context.Background()
	st := app.storeHandle()
	for k, v := range map[string]string{
		store.PlatformSettingTurnstileEnabled: "1",
		store.PlatformSettingTurnstileSiteKey: "site",
		store.PlatformSettingTurnstileSecret:  "secret",
	} {
		if err := st.SetPlatformSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	app.turnstileVerifier = hostedauth.NewTurnstileVerifier(&http.Client{Transport: fakeTurnstile{}})
}
