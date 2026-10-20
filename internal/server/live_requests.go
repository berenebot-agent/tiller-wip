package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// Live request metadata deliberately excludes all body content, including
// opt-in detailed errors. The existing Activity history remains its boundary.
type liveRequestView struct {
	globalActivityView
	Revision    uint64               `json:"revision"`
	State       string               `json:"state"`
	CompletedAt string               `json:"completed_at,omitempty"`
	Attempts    []requestAttemptView `json:"attempts"`
}

func (s *Server) publishLiveRequest(row *logRow, completed bool) {
	if s.liveHub == nil || !row.liveEnabled {
		return
	}
	row.liveRevision++
	v := liveRequestView{
		globalActivityView: globalActivityView{
			activityView: activityView{
				ID: row.clientRequestID, ClientRequestID: row.clientRequestID,
				RequestedModel: row.requestedModel, ExposedModel: row.exposedModel,
				RouteKind: row.routeKind, RouteModelID: row.routeModelID, RouteModel: row.routeModel,
				ResolvedProvider: row.resolvedProvider, ResolvedModel: row.resolvedModel,
				Protocol: row.protocol, Streaming: row.streaming, HTTPStatus: row.httpStatus,
				LatencyMs: row.latencyMs, InputTokens: row.inputTokens, OutputTokens: row.outputTokens,
				CacheReadInputTokens: row.cacheReadInputTokens, CacheCreationInputTokens: row.cacheCreationInputTokens,
				InputTokensSource: row.inputTokensSource, OutputTokensSource: row.outputTokensSource,
				ProviderCostMicros: row.providerCostMicros, EstimatedCostMicros: row.estimatedCostMicros, ProviderRequestID: row.providerRequestID,
				ErrorText: row.errorText, FallbackUsed: row.liveFallback, CreatedAt: row.createdAt,
			},
			ClientKeyID: row.clientKeyID, ClientName: row.clientName,
		},
		Revision: row.liveRevision, State: "running", Attempts: []requestAttemptView{},
	}
	for i, a := range row.attempts {
		v.Attempts = append(v.Attempts, requestAttemptView{AttemptNumber: i + 1, Provider: a.provider, Model: a.model, Result: a.result, HTTPStatus: &a.httpStatus, FailureClass: strPtrIfNonEmpty(a.failureClass), LatencyMs: a.latencyMs})
	}
	if !completed && row.liveAttempt != nil {
		a := row.liveAttempt
		v.ResolvedProvider, v.ResolvedModel = strPtr(a.provider), strPtr(a.model)
		if len(v.Attempts) == 0 || v.Attempts[len(v.Attempts)-1].Result != "success" {
			v.Attempts = append(v.Attempts, requestAttemptView{AttemptNumber: len(v.Attempts) + 1, Provider: a.provider, Model: a.model, Result: "running"})
		} else {
			v.Attempts[len(v.Attempts)-1].Result = "running"
		}
	}
	if completed {
		v.State = "success"
		if row.httpStatus < 200 || row.httpStatus >= 300 || row.errorText != nil {
			v.State = "failed"
		}
		if row.errorText != nil && (*row.errorText == "client_cancelled" || *row.errorText == "client_timeout") {
			v.State = "cancelled"
		}
		v.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	} else if row.streaming {
		v.State = "streaming"
	}
	v.AttemptCount, v.AttemptRows = len(v.Attempts), len(v.Attempts)
	s.liveHub.putRequest(row.accountID, v)
}

func (h *liveHub) putRequest(accountID string, v liveRequestView) {
	// Store and enqueue under the same lock so concurrent request publications
	// cannot reorder revisions. A dropped delta is repaired by snapshots.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.requests == nil {
		h.requests = map[string]map[string]liveRequestView{}
	}
	if h.requests[accountID] == nil {
		h.requests[accountID] = map[string]liveRequestView{}
	}
	h.requests[accountID][v.ID] = v
	// Prune once per publication. The stored row is needed even with nobody
	// watching: a pane opened mid-request is filled from the snapshot. Only the
	// fan-out copy is skipped when the account has no subscriber.
	h.pruneRequestsLocked(time.Now())
	if len(h.subs[accountID]) == 0 {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte("event: request\ndata: " + string(data) + "\n\n")
	for ch := range h.subs[accountID] {
		select {
		case ch <- msg:
		default:
		}
	}
}

func (h *liveHub) withdrawRequest(accountID, id string) {
	h.mu.Lock()
	delete(h.requests[accountID], id)
	h.mu.Unlock()
	h.broadcast(accountID, "request", map[string]any{"id": id, "state": "removed"})
}

func (h *liveHub) pruneRequestsLocked(now time.Time) {
	for account, rows := range h.requests {
		completed := []liveRequestView{}
		for id, row := range rows {
			if row.CompletedAt == "" {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, row.CompletedAt)
			if now.Sub(at) >= 30*time.Second {
				delete(rows, id)
			} else {
				completed = append(completed, row)
			}
		}
		sort.Slice(completed, func(i, j int) bool {
			a, _ := time.Parse(time.RFC3339Nano, completed[i].CompletedAt)
			b, _ := time.Parse(time.RFC3339Nano, completed[j].CompletedAt)
			return a.After(b)
		})
		for i := 200; i < len(completed); i++ {
			delete(rows, completed[i].ID)
		}
		if len(rows) == 0 {
			delete(h.requests, account)
		}
	}
}

func (h *liveHub) requestSnapshot(accountID string, activeOnly bool) []liveRequestView {
	out := []liveRequestView{}
	if h == nil {
		return out
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pruneRequestsLocked(time.Now())
	for _, row := range h.requests[accountID] {
		if !activeOnly || row.CompletedAt == "" {
			out = append(out, row)
		}
	}
	return out
}

func (s *Server) liveRequests(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"data": s.liveHub.requestSnapshot(s.scope(r).AccountID(), true), "generated_at": time.Now().UTC().Format(time.RFC3339Nano)})
}

// Reconcile only explicitly tracked IDs, never load unrelated completed history.
// Finished rows beyond the small in-memory window come from account-scoped
// Activity storage. Missing rows are explicit, not guessed success/failure.
func (s *Server) reconcileLiveRequests(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []string `json:"ids"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if json.NewDecoder(r.Body).Decode(&input) != nil || len(input.IDs) > 1000 {
		adminError(w, 400, "invalid_request", "Supply at most 1000 tracked request IDs.")
		return
	}
	sc := s.scope(r)
	data := []liveRequestView{}
	known := map[string]liveRequestView{}
	for _, v := range s.liveHub.requestSnapshot(sc.AccountID(), false) {
		known[v.ID] = v
	}
	missing := []string{}
	for _, id := range input.IDs {
		if v, ok := known[id]; ok {
			data = append(data, v)
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		_ = s.flushActivity(r.Context())
		rows, err := sc.LiveActivityByIDs(r.Context(), missing)
		if err != nil {
			activityReadError(w, err, "Could not reconcile live requests.")
			return
		}
		for _, row := range rows {
			enabled, err := sc.ClientKeyLoggingEnabled(r.Context(), row.ClientKeyID)
			if err != nil || !enabled {
				continue
			}
			v := liveRequestView{globalActivityView: globalActivityView{activityView: activityViewFromStore(row), ClientKeyID: row.ClientKeyID, ClientName: row.ClientName}, State: "success", Attempts: []requestAttemptView{}}
			v.RequestBody, v.ErrorBody, v.ErrorMessage = nil, nil, nil
			if row.HTTPStatus < 200 || row.HTTPStatus >= 300 || row.ErrorText != nil {
				v.State = "failed"
			}
			if row.ErrorText != nil && (*row.ErrorText == "client_cancelled" || *row.ErrorText == "client_timeout") {
				v.State = "cancelled"
			}
			at, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
			if err == nil {
				v.CompletedAt = at.Add(time.Duration(row.LatencyMs) * time.Millisecond).Format(time.RFC3339Nano)
			}
			attempts, err := sc.ListRequestAttempts(r.Context(), row.ID)
			if err != nil {
				activityReadError(w, err, "Could not reconcile request attempts.")
				return
			}
			for _, a := range attempts {
				v.Attempts = append(v.Attempts, requestAttemptView{AttemptNumber: a.AttemptNumber, Provider: a.Provider, Model: a.Model, Result: a.Result, HTTPStatus: a.HTTPStatus, FailureClass: a.FailureClass, LatencyMs: a.LatencyMs})
			}
			data = append(data, v)
			known[row.ID] = v
		}
	}
	notFound := []string{}
	for _, id := range missing {
		if _, ok := known[id]; !ok {
			notFound = append(notFound, id)
		}
	}
	writeJSON(w, 200, map[string]any{"data": data, "missing": notFound, "generated_at": time.Now().UTC().Format(time.RFC3339Nano)})
}
