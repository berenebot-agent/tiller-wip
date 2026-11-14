package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
)

// clientsView returns everything the console's Clients screen needs in one
// response: the client keys (honouring ?search= and ?group=), the full model
// list, virtual models, providers and catalogue templates.
//
// The screen used to issue these five reads separately on every render, and
// since the account budget is charged per console request that cost five units
// per navigation, cancel and save. One round trip costs one unit.
//
// Each part is produced by the existing list handler, run against a derived
// request, so filtering, pagination limits and response shapes cannot drift
// from the standalone endpoints. Every part is the handler's untouched JSON
// body. The first non-200 part is relayed as-is and nothing else is written.
//
// This route is cookie-session only (requireAdmin). The standalone endpoints
// stay per-area scoped for API tokens; a combined route would need to check
// three areas at once, and the console is its only caller.
func (s *Server) clientsView(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientQuery := url.Values{"limit": {"200"}, "search": {q.Get("search")}, "group": {q.Get("group")}}
	parts := []struct {
		key     string
		query   url.Values
		handler http.HandlerFunc
	}{
		{"client_keys", clientQuery, s.listClientKeys},
		{"models", url.Values{"all": {"1"}}, s.listAllModels},
		{"virtual_models", url.Values{"limit": {"200"}}, s.listVirtualModels},
		{"providers", url.Values{"limit": {"200"}}, s.listProviders},
		{"templates", url.Values{}, s.listTemplates},
	}
	out := make(map[string]json.RawMessage, len(parts))
	for _, part := range parts {
		sub := r.Clone(r.Context())
		u := *r.URL
		u.RawQuery = part.query.Encode()
		sub.URL = &u
		rec := httptest.NewRecorder()
		part.handler(rec, sub)
		if rec.Code != http.StatusOK {
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		out[part.key] = json.RawMessage(rec.Body.Bytes())
	}
	writeJSON(w, http.StatusOK, out)
}
