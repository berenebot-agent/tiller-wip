package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tiller-router/tiller-router/internal/config"
	"github.com/tiller-router/tiller-router/internal/database"
)

// deepSeekRoleUpstream emulates the one DeepSeek behaviour under test: its Chat
// surface accepts only system/user/assistant/tool and answers a request carrying
// OpenAI's `developer` role with HTTP 422 before inference runs. The generic
// OpenAI-compatible sibling accepts the same body untouched, so the pair proves
// the rewrite is provider-scoped rather than applied to every Chat target.
type deepSeekRoleUpstream struct {
	mu       sync.Mutex
	received map[string]map[string]any
}

func (u *deepSeekRoleUpstream) record(target string, body map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.received[target] = body
}

func (u *deepSeekRoleUpstream) body(target string) map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.received[target]
}

func (u *deepSeekRoleUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		target, upstreamPath := "", strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case strings.HasPrefix(upstreamPath, "/ds/v1/"):
			target, upstreamPath = "deepseek", strings.TrimPrefix(upstreamPath, "/ds/v1")
		case strings.HasPrefix(upstreamPath, "/open/v1/"):
			target, upstreamPath = "openai", strings.TrimPrefix(upstreamPath, "/open/v1")
		default:
			http.NotFound(w, r)
			return
		}
		if upstreamPath == "/models" {
			model := map[string]any{"id": "deepseek-flash"}
			if target == "openai" {
				model = map[string]any{"id": "open-flash"}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{model}})
			return
		}
		if upstreamPath != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if target == "deepseek" {
			if messages, ok := input["messages"].([]any); ok {
				for _, raw := range messages {
					message, _ := raw.(map[string]any)
					if message["role"] == "developer" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = io.WriteString(w, `{"error":{"message":"Failed to deserialize the JSON body into the target type: messages[0].role: unknown variant `+"`developer`"+`, expected one of `+"`system`"+`, `+"`user`"+`, `+"`assistant`"+`, `+"`tool`"+`"}}`)
						return
					}
				}
			}
		}
		u.record(target, input)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chat-fixture", "object": "chat.completion", "model": input["model"],
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
		})
	}
}

// TestDeepSeekTargetRewritesDeveloperRole drives the whole gateway path — client
// key, real model, provider-scoped normalisation, upstream request — because a
// unit test on normalizeDeepSeekRequest cannot show that client.go dispatches it
// for a `deepseek` Chat target while leaving every other target's body alone.
func TestDeepSeekTargetRewritesDeveloperRole(t *testing.T) {
	t.Parallel()
	upstream := &deepSeekRoleUpstream{received: map[string]map[string]any{}}
	mock := httptest.NewServer(upstream.handler(t))
	t.Cleanup(mock.Close)

	db, err := database.Open(context.Background(), filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	app := newTestServer(t, config.Config{TillerUser: "admin", TillerUserPassword: "deepseek-role-admin", DataDir: t.TempDir(), ListenAddr: ":8080"}, db)
	router := httptest.NewServer(app.Handler())
	t.Cleanup(router.Close)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &testAPI{t: t, base: router.URL, client: &http.Client{Jar: jar}, server: app}
	admin := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		status, payload, _ := api.request(method, path, body)
		if status != want {
			t.Fatalf("%s %s: status %d, want %d (%v)", method, path, status, want, payload)
		}
		return payload
	}
	login := admin("POST", "/api/admin/session", map[string]any{"username": "admin", "password": "deepseek-role-admin"}, 200)
	api.csrf = login["csrf_token"].(string)

	modelID := func(name, providerType, basePath, upstreamModel string) string {
		t.Helper()
		provider := admin("POST", "/api/admin/providers", map[string]any{
			"name": name, "type": providerType, "base_url": mock.URL + basePath, "credential": "upstream-secret",
		}, 201)
		models := admin("GET", "/api/admin/providers/"+provider["id"].(string)+"/models", nil, 200)
		for _, raw := range models["data"].([]any) {
			model := raw.(map[string]any)
			if model["upstream_model_id"] == upstreamModel {
				return model["id"].(string)
			}
		}
		t.Fatalf("%s did not expose %s", providerType, upstreamModel)
		return ""
	}
	deepseekModel := modelID("deepseek-provider", "deepseek", "/ds/v1", "deepseek-flash")
	openaiModel := modelID("openai-provider", "generic-openai", "/open/v1", "open-flash")

	key := admin("POST", "/api/admin/client-keys", map[string]any{"name": "role normalisation", "type": "catalogue"}, 201)
	admin("PUT", "/api/admin/client-keys/"+key["id"].(string)+"/permissions", map[string]any{
		"defaults": []any{},
		"permissions": []any{
			map[string]any{"kind": "real", "model_id": deepseekModel, "enabled": true},
			map[string]any{"kind": "real", "model_id": openaiModel, "enabled": true},
		},
	}, 204)
	secret := key["secret"].(string)

	request := func(model string) int {
		t.Helper()
		resp, _ := clientCall(t, api.base, secret, "/v1/chat/completions", map[string]any{
			"model": model,
			"messages": []any{
				map[string]any{"role": "developer", "content": "harness rules"},
				map[string]any{"role": "user", "content": "hello"},
			},
		})
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	if status := request("deepseek-provider/deepseek-flash"); status != 200 {
		t.Fatalf("DeepSeek target answered %d: the developer role never reached it as system", status)
	}
	rewritten := upstream.body("deepseek")
	if rewritten == nil {
		t.Fatal("DeepSeek upstream received no request")
	}
	roles := messageValues(rewritten, "role")
	if len(roles) != 2 || roles[0] != "system" || roles[1] != "user" {
		t.Fatalf("DeepSeek roles = %v, want [system user]", roles)
	}
	if content := messageValues(rewritten, "content"); len(content) != 2 || content[0] != "harness rules" {
		t.Fatalf("DeepSeek message content = %v, want the caller's text preserved", content)
	}

	if status := request("openai-provider/open-flash"); status != 200 {
		t.Fatalf("OpenAI-compatible target answered %d", status)
	}
	forwarded := upstream.body("openai")
	if forwarded == nil {
		t.Fatal("OpenAI-compatible upstream received no request")
	}
	if roles := messageValues(forwarded, "role"); len(roles) != 2 || roles[0] != "developer" || roles[1] != "user" {
		t.Fatalf("OpenAI-compatible roles = %v, want [developer user]: the rewrite must be DeepSeek-scoped", roles)
	}
}
