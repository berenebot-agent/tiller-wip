package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/config"
)

// managementGuide serves the management API usage guide at GET /api and
// GET /api/admin (local and hosted). It is public by design — it contains no
// secrets and exists so an operator can point an agent at one URL — and
// content-negotiates: an `Accept: text/markdown` client (an agent or CLI)
// receives markdown; everyone else gets a styled HTML page. The exact-match
// routes never shadow the API surface itself (GET /api/admin/providers etc.).
func (s *Server) managementGuide(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/markdown") {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(renderManagementGuideMarkdown(s.managementGuideInfo())))
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		s.managementDiscovery(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(renderManagementGuideHTML(s.managementGuideInfo())))
}

// managementDiscovery serves the machine-readable discovery document. It is
// reachable at GET /api.json and at GET /api when the client asks for JSON
// (Accept: application/json), so a host SDK can probe the instance and learn
// how to talk to it with no out-of-band knowledge.
func (s *Server) managementDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	payload := map[string]any{
		"name":        "Tiller Router",
		"api_version": "v1",
		"api_base":    "/api/admin",
		"auth": map[string]string{
			"scheme":     "bearer",
			"header":     "Authorization",
			"key_prefix": auth.ManagementTokenPrefix,
		},
		"agent_guide": "/api/admin",
		"reference":   "/api/admin",
		"scopes":      map[string]any{"areas": auth.ManagementScopeAreas(), "tiers": []string{"read", "write", "delete"}},
		"capabilities": []string{
			"providers", "models", "virtual-models", "virtual-groups",
			"client-keys", "client-key-permissions", "settings",
			"usage", "activity", "cooldown", "health",
		},
		"endpoints": "see GET /api/admin (text/markdown) for the full route reference",
		"openapi":   "/api/openapi.json",
		"mode":      string(s.config.Mode),
		"limits":    s.discoveryLimits(),
	}
	if s.config.Mode == config.ModeHosted {
		payload["token_management"] = "dashboard-only"
		payload["scopes"] = map[string]any{"areas": auth.ManagementScopeAreas(), "tiers": []string{"read", "write"}}
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// discoveryLimits advertises the bounds a client needs to page and retry
// safely. The values come from the same constants the server enforces, so the
// advertised numbers cannot drift from behaviour.
func (s *Server) discoveryLimits() map[string]any {
	limits := map[string]any{
		"page_size_default":               50,
		"page_size_max":                   200,
		"admin_request_body_bytes":        32 << 20,
		"offset_max":                      10000,
		"activity_retention_days_default": 30,
	}
	if s.config.Mode == config.ModeHosted {
		limits["requests_per_minute_per_account"] = hostedManagementRequestsPerMinute
		limits["max_tokens_per_account"] = hostedMaxManagementTokens
	}
	return limits
}

// managementGuideScope is one row of the area × tier scope table.
type managementGuideScope struct{ Area, Read, Write, Delete string }

var managementGuideScopes = []managementGuideScope{
	{"Providers", "inspect providers, models, OAuth status", "create/edit providers, replace credentials, refresh discovery, connect OAuth, add manual models", "delete providers/models, disconnect OAuth"},
	{"Virtual models", "inspect routes", "create/edit groups and virtual models", "delete groups and virtual models"},
	{"Client keys", "list keys and permissions", "create/edit/rotate keys, edit permissions", "delete keys"},
	{"Settings", "read settings", "change settings, send a test notification", "—"},
	{"Monitoring", "usage, quota, activity (+CSV exports), cooldown status, health", "—", "clear activity, clear cooldown"},
}

// managementRoute is one token-reachable route, recorded as it is registered
// so the guide, discovery document and OpenAPI file are generated from the
// routes the server actually enforces.
type managementRoute struct {
	Method string
	Path   string
	Area   string
	Tier   int
}

// handleManage registers a token-reachable admin route and records it.
func (s *Server) handleManage(mux *http.ServeMux, pattern, area string, tier int, next http.Handler) {
	method, path, _ := strings.Cut(pattern, " ")
	s.managementRoutes = append(s.managementRoutes, managementRoute{Method: method, Path: path, Area: area, Tier: tier})
	mux.Handle(pattern, s.requireManage(area, tier, next))
}

// managementGuideAreaOrder fixes the section order and display names.
var managementGuideAreaOrder = []struct{ Key, Name string }{
	{"providers", "Providers"},
	{"virtual", "Virtual models"},
	{"client_keys", "Client keys"},
	{"settings", "Settings"},
	{"monitoring", "Monitoring"},
}

// managementGuideInfo is what the renderers need beyond static text.
type managementGuideInfo struct {
	Hosted bool
	Routes []managementRoute
}

func (s *Server) managementGuideInfo() managementGuideInfo {
	return managementGuideInfo{Hosted: s.config.Mode == config.ModeHosted, Routes: s.managementRoutes}
}

type managementGuideArea struct {
	Name   string
	Routes []managementRoute
}

// areas groups the routes the caller's tokens can reach (hosted tokens stop at
// the write tier) by area, ordered read → write → delete.
func (i managementGuideInfo) areas() []managementGuideArea {
	var out []managementGuideArea
	for _, area := range managementGuideAreaOrder {
		var rows []managementRoute
		for _, r := range i.Routes {
			if r.Area != area.Key || (i.Hosted && r.Tier > hostedMaxManagementScopeTier) {
				continue
			}
			rows = append(rows, r)
		}
		sort.SliceStable(rows, func(a, b int) bool { return rows[a].Tier < rows[b].Tier })
		out = append(out, managementGuideArea{Name: area.Name, Routes: rows})
	}
	return out
}

// deleteCell is the scope-table delete column; hosted tokens cannot hold it.
func (i managementGuideInfo) deleteCell(row managementGuideScope) string {
	if i.Hosted {
		return "not available to hosted tokens"
	}
	return row.Delete
}

func managementTierLabel(tier int) string {
	switch tier {
	case auth.ScopeRead:
		return "read"
	case auth.ScopeWrite:
		return "write"
	default:
		return "delete"
	}
}

func renderManagementGuideMarkdown(info managementGuideInfo) string {
	var b strings.Builder
	b.WriteString("# Tiller Router — Management API\n\n")
	b.WriteString("The Tiller dashboard is a client of this JSON REST API. Scripts and agents can\n")
	b.WriteString("drive the same endpoints with a management API token — no browser session,\n")
	b.WriteString("no cookies, no CSRF token.\n\n")
	b.WriteString("## Authentication\n\n")
	b.WriteString("Create a token in the dashboard under **Settings → Management API**. Send it\n")
	b.WriteString("on every request as a bearer credential:\n\n")
	b.WriteString("```\nAuthorization: Bearer mt-tr-<selector>.<secret>\n```\n\n")
	b.WriteString("Tokens are shown once at creation and stored only as a hash. They may carry an\n")
	b.WriteString("expiry, and can be rotated or revoked at any time. A token can introspect\n")
	b.WriteString("itself at `GET /api/admin/api-tokens/self` (name, scopes, expiry, last use).\n\n")
	if info.Hosted {
		b.WriteString("**Hosted accounts:** creating, rotating or widening a token asks for your\n")
		b.WriteString("password (or a fresh passkey/Google check); new tokens default to a 30-day\n")
		b.WriteString("expiry; tokens are limited to `read` and `write` (destructive deletes stay in\n")
		b.WriteString("the dashboard); an account can hold up to 10 tokens; and all of an account's\n")
		b.WriteString("tokens share a budget of 60 requests per minute.\n\n")
	}
	b.WriteString("## Scopes\n\n")
	b.WriteString("Each token grants per-area access with three escalating levels:\n")
	b.WriteString("`read` ⊂ `write` ⊂ `delete`. A `write` grant satisfies `read`; `delete`\n")
	b.WriteString("satisfies both.\n\n")
	b.WriteString("| Area | read | write | delete |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, row := range managementGuideScopes {
		b.WriteString("| " + row.Area + " | " + row.Read + " | " + row.Write + " | " + info.deleteCell(row) + " |\n")
	}
	b.WriteString("\nIf a request exceeds the token's ceiling the API answers `403` with code\n")
	b.WriteString("`insufficient_scope` and a `required` field naming the needed grant (for\n")
	b.WriteString("example `providers:write`).\n\n")
	b.WriteString("## Endpoint reference\n\n")
	b.WriteString("All endpoints are relative to this instance's origin; `{id}` is a resource id\n")
	b.WriteString("returned by the corresponding list endpoint.\n")
	for _, area := range info.areas() {
		b.WriteString("\n### " + area.Name + "\n\n")
		b.WriteString("| Method | Path | Required tier |\n")
		b.WriteString("|---|---|---|\n")
		for _, ep := range area.Routes {
			b.WriteString("| " + ep.Method + " | `" + ep.Path + "` | " + managementTierLabel(ep.Tier) + " |\n")
		}
	}
	b.WriteString("\n## Reading usage\n\n")
	b.WriteString("`GET /api/admin/usage` groups token, cache, and cost figures by client key id,\n")
	b.WriteString("virtual model, and real model (`provider/model`). Virtual- and real-model keys\n")
	b.WriteString("are already display names; client-key aggregates are keyed by the opaque\n")
	b.WriteString("`client_key_id`. The same response carries `client_key_names`, a `{id: name}`\n")
	b.WriteString("map, so you can label client rows without a second request. An id missing from\n")
	b.WriteString("that map belongs to a client key that has since been deleted (its history is\n")
	b.WriteString("retained).\n\n")
	b.WriteString("## Paging and limits\n\n")
	b.WriteString("List endpoints accept `limit` (default 50, maximum 200), `offset` (capped at\n")
	b.WriteString("10000) and, where supported, `search`. To read further than that, narrow with\n")
	b.WriteString("`search` or use the CSV export routes. Admin request bodies are limited to\n")
	b.WriteString("32 MiB.\n\n")
	if info.Hosted {
		b.WriteString("On hosted accounts, provider base URLs must be public: addresses that resolve to\n")
		b.WriteString("private or internal networks are rejected, and some provider types are not\n")
		b.WriteString("offered. Token-creation notices are emailed only when the instance has mail\n")
		b.WriteString("configured.\n\n")
	}
	b.WriteString("## Errors\n\n")
	b.WriteString("Failures use the envelope `{\"error\":{\"code\":\"...\",\"message\":\"...\"}}`:\n\n")
	b.WriteString("- `401 invalid_api_token` — missing, unknown, disabled, or malformed token.\n")
	b.WriteString("- `401 token_expired` — the token's expiry has passed.\n")
	b.WriteString("- `403 insufficient_scope` — the token's ceiling is below the route's tier.\n")
	b.WriteString("- `429 authentication_rate_limited` — too many failed attempts; retry after `Retry-After` seconds.\n")
	if info.Hosted {
		b.WriteString("- `429 rate_limited` — the account's 60 requests/minute management budget is spent; retry after `Retry-After` seconds.\n")
	}
	b.WriteString("\n")
	b.WriteString("## Examples\n\n")
	b.WriteString("```bash\n")
	b.WriteString("TILLER=https://your-tiller            # replace with this instance's origin\n")
	b.WriteString("TOKEN=mt-tr-...                       # the token shown when you created it\n\n")
	b.WriteString("# List providers and their discovered models\n")
	b.WriteString("curl -H \"Authorization: Bearer $TOKEN\" \"$TILLER/api/admin/providers\"\n\n")
	b.WriteString("# Point a virtual model at an upstream model\n")
	b.WriteString("curl -H \"Authorization: Bearer $TOKEN\" -H 'Content-Type: application/json' \\\n")
	b.WriteString("  -d '{\"group_name\":\"main\",\"name\":\"coding\",\"targets\":[{\"provider_model_id\":\"<model-id>\",\"enabled\":true}]}' \\\n")
	b.WriteString("  \"$TILLER/api/admin/virtual-models\"\n\n")
	b.WriteString("# Create a client key for an inference client\n")
	b.WriteString("curl -H \"Authorization: Bearer $TOKEN\" -H 'Content-Type: application/json' \\\n")
	b.WriteString("  -d '{\"name\":\"hermes\",\"type\":\"catalogue\"}' \\\n")
	b.WriteString("  \"$TILLER/api/admin/client-keys\"\n```\n\n")
	b.WriteString("## Out of reach for tokens\n\n")
	b.WriteString("Token lifecycle itself (list/create/edit/rotate/revoke) is browser-only, so a\n")
	b.WriteString("compromised token cannot mint siblings or widen its own scopes. Backup export,\n")
	b.WriteString("account export, identity operations (password, passkeys, email, account\n")
	b.WriteString("deletion, sessions), setup, onboarding, debug endpoints, and the live SSE\n")
	b.WriteString("stream are also never token-reachable.\n")
	return b.String()
}

func renderManagementGuideHTML(info managementGuideInfo) string {
	var b strings.Builder
	b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n<title>Tiller Router — Management API</title>\n<link rel=\"stylesheet\" href=\"/style.css\">\n</head>\n<body class=\"api-guide-body\">\n<main class=\"api-guide\">\n")
	b.WriteString("<p class=\"eyebrow\">TILLER ROUTER</p>\n<h1>Management API</h1>\n")
	b.WriteString("<p class=\"api-guide-lede\">The Tiller dashboard is a client of this JSON REST API. Scripts and agents can drive the same endpoints with a management API token — no browser session, no cookies, no CSRF token.</p>\n")

	b.WriteString("<h2>Authentication</h2>\n")
	b.WriteString("<p>Create a token in the dashboard under <strong>Settings → Management API</strong>. Send it on every request as a bearer credential:</p>\n")
	b.WriteString("<pre><code>Authorization: Bearer mt-tr-&lt;selector&gt;.&lt;secret&gt;</code></pre>\n")
	b.WriteString("<p>Tokens are shown once at creation and stored only as a hash. They may carry an expiry, and can be rotated or revoked at any time. A token can introspect itself at <code>GET /api/admin/api-tokens/self</code> (name, scopes, expiry, last use).</p>\n")
	if info.Hosted {
		b.WriteString("<p><strong>Hosted accounts:</strong> creating, rotating or widening a token asks for your password (or a fresh passkey/Google check); new tokens default to a 30-day expiry; tokens are limited to <code>read</code> and <code>write</code> (destructive deletes stay in the dashboard); an account can hold up to 10 tokens; and all of an account's tokens share a budget of 60 requests per minute.</p>\n")
	}

	b.WriteString("<h2>Scopes</h2>\n")
	b.WriteString("<p>Each token grants per-area access with three escalating levels: <code>read</code> ⊂ <code>write</code> ⊂ <code>delete</code>. A <code>write</code> grant satisfies <code>read</code>; <code>delete</code> satisfies both.</p>\n")
	b.WriteString("<table class=\"api-guide-table\"><thead><tr><th>Area</th><th>read</th><th>write</th><th>delete</th></tr></thead><tbody>\n")
	for _, row := range managementGuideScopes {
		b.WriteString("<tr><td><strong>" + row.Area + "</strong></td><td>" + row.Read + "</td><td>" + row.Write + "</td><td>" + info.deleteCell(row) + "</td></tr>\n")
	}
	b.WriteString("</tbody></table>\n")
	b.WriteString("<p>If a request exceeds the token's ceiling the API answers <code>403</code> with code <code>insufficient_scope</code> and a <code>required</code> field naming the needed grant (for example <code>providers:write</code>).</p>\n")

	b.WriteString("<h2>Endpoint reference</h2>\n")
	b.WriteString("<p>All endpoints are relative to this instance's origin; <code>{id}</code> is a resource id returned by the corresponding list endpoint.</p>\n")
	for _, area := range info.areas() {
		b.WriteString("<h3>" + area.Name + "</h3>\n")
		b.WriteString("<table class=\"api-guide-table\"><thead><tr><th>Method</th><th>Path</th><th>Required tier</th></tr></thead><tbody>\n")
		for _, ep := range area.Routes {
			b.WriteString("<tr><td><code>" + ep.Method + "</code></td><td><code>" + ep.Path + "</code></td><td>" + managementTierLabel(ep.Tier) + "</td></tr>\n")
		}
		b.WriteString("</tbody></table>\n")
	}

	b.WriteString("<h2>Reading usage</h2>\n")
	b.WriteString("<p><code>GET /api/admin/usage</code> groups token, cache, and cost figures by client key id, virtual model, and real model (<code>provider/model</code>). Virtual- and real-model keys are already display names; client-key aggregates are keyed by the opaque <code>client_key_id</code>. The same response carries <code>client_key_names</code>, a <code>{id: name}</code> map, so you can label client rows without a second request. An id missing from that map belongs to a client key that has since been deleted (its history is retained).</p>\n")
	b.WriteString("<h2>Paging and limits</h2>\n")
	b.WriteString("<p>List endpoints accept <code>limit</code> (default 50, maximum 200), <code>offset</code> (capped at 10000) and, where supported, <code>search</code>. To read further than that, narrow with <code>search</code> or use the CSV export routes. Admin request bodies are limited to 32 MiB.</p>\n")
	if info.Hosted {
		b.WriteString("<p>On hosted accounts, provider base URLs must be public: addresses that resolve to private or internal networks are rejected, and some provider types are not offered. Token-creation notices are emailed only when the instance has mail configured.</p>\n")
	}
	b.WriteString("<h2>Errors</h2>\n")
	b.WriteString("<p>Failures use the envelope <code>{\"error\":{\"code\":\"...\",\"message\":\"...\"}}</code>:</p>\n")
	b.WriteString("<ul>\n<li><code>401 invalid_api_token</code> — missing, unknown, disabled, or malformed token.</li>\n<li><code>401 token_expired</code> — the token's expiry has passed.</li>\n<li><code>403 insufficient_scope</code> — the token's ceiling is below the route's tier.</li>\n<li><code>429 authentication_rate_limited</code> — too many failed attempts; retry after <code>Retry-After</code> seconds.</li>\n")
	if info.Hosted {
		b.WriteString("<li><code>429 rate_limited</code> — the account's 60 requests/minute management budget is spent; retry after <code>Retry-After</code> seconds.</li>\n")
	}
	b.WriteString("</ul>\n")

	b.WriteString("<h2>Examples</h2>\n")
	b.WriteString("<pre><code>TILLER=https://your-tiller            # replace with this instance's origin\nTOKEN=mt-tr-...                       # the token shown when you created it\n\n# List providers and their discovered models\ncurl -H \"Authorization: Bearer $TOKEN\" \"$TILLER/api/admin/providers\"\n\n# Point a virtual model at an upstream model\ncurl -H \"Authorization: Bearer $TOKEN\" -H 'Content-Type: application/json' \\\n  -d '{\"group_name\":\"main\",\"name\":\"coding\",\"targets\":[{\"provider_model_id\":\"&lt;model-id&gt;\",\"enabled\":true}]}' \\\n  \"$TILLER/api/admin/virtual-models\"\n\n# Create a client key for an inference client\ncurl -H \"Authorization: Bearer $TOKEN\" -H 'Content-Type: application/json' \\\n  -d '{\"name\":\"hermes\",\"type\":\"catalogue\"}' \\\n  \"$TILLER/api/admin/client-keys\"</code></pre>\n")

	b.WriteString("<h2>Out of reach for tokens</h2>\n")
	b.WriteString("<p>Token lifecycle itself (list/create/edit/rotate/revoke) is browser-only, so a compromised token cannot mint siblings or widen its own scopes. Backup export, account export, identity operations (password, passkeys, email, account deletion, sessions), setup, onboarding, debug endpoints, and the live SSE stream are also never token-reachable.</p>\n")
	b.WriteString("</main>\n</body>\n</html>\n")
	return b.String()
}
