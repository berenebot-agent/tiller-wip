package server

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

var openAPIPathParam = regexp.MustCompile(`\{([^}]+)\}`)
var openAPIIDChars = regexp.MustCompile(`[^A-Za-z0-9]+`)

// managementOpenAPI serves a minimal OpenAPI 3.1 description of the
// token-reachable management API at GET /api/openapi.json. It is generated
// from the routes recorded by handleManage, so paths, methods and required
// scopes always match what the server enforces. Request and response bodies
// are intentionally not schematised: see the guide at /api/admin for payload
// examples.
func (s *Server) managementOpenAPI(w http.ResponseWriter, _ *http.Request) {
	info := s.managementGuideInfo()
	paths := map[string]map[string]any{}
	areaNames := map[string]string{}
	for _, a := range managementGuideAreaOrder {
		areaNames[a.Key] = a.Name
	}
	routes := append([]managementRoute(nil), info.Routes...)
	sort.SliceStable(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
	for _, r := range routes {
		if info.Hosted && r.Tier > hostedMaxManagementScopeTier {
			continue
		}
		op := map[string]any{
			"operationId": strings.ToLower(r.Method) + strings.Trim(openAPIIDChars.ReplaceAllString(r.Path, "_"), "_"),
			"summary":     r.Method + " " + r.Path,
			"tags":        []string{areaNames[r.Area]},
			"security":    []map[string][]string{{"managementToken": {}}},
			// Vendor extension naming the token grant this route requires.
			"x-required-scope": r.Area + ":" + managementTierLabel(r.Tier),
			"responses": map[string]any{
				"200": map[string]any{"description": "Success (JSON, or CSV for export routes)."},
				"401": map[string]any{"description": "Missing, invalid or expired token."},
				"403": map[string]any{"description": "insufficient_scope: the token's ceiling is below the route's tier."},
				"429": map[string]any{"description": "Rate limited; retry after Retry-After seconds."},
			},
		}
		var params []map[string]any
		for _, m := range openAPIPathParam.FindAllStringSubmatch(r.Path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
		}
		if params != nil {
			op["parameters"] = params
		}
		if paths[r.Path] == nil {
			paths[r.Path] = map[string]any{}
		}
		paths[r.Path][strings.ToLower(r.Method)] = op
	}
	// The one token-accessible lifecycle route: self-introspection.
	paths["/api/admin/api-tokens/self"] = map[string]any{"get": map[string]any{
		"operationId": "getapi_tokens_self", "summary": "GET /api/admin/api-tokens/self", "tags": []string{"Tokens"},
		"security":  []map[string][]string{{"managementToken": {}}},
		"responses": map[string]any{"200": map[string]any{"description": "The calling token's name, scopes, expiry and last use."}, "401": map[string]any{"description": "Missing, invalid or expired token."}},
	}}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Tiller Router Management API",
			"version":     "v1",
			"description": "Drive the Tiller dashboard's JSON API with a management token. Human guide: GET /api/admin. Token lifecycle is dashboard-only.",
		},
		"paths": paths,
		"components": map[string]any{"securitySchemes": map[string]any{
			"managementToken": map[string]any{"type": "http", "scheme": "bearer", "description": "Management API token (mt-tr-...), created under Settings → Management API."},
		}},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(doc)
}
