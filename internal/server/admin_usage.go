package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

// usageWindows, cacheWindows and targetResolutionHealth are the store's
// account-scoped aggregate types; aliased here so the JSON envelope is
// unchanged.
type usageWindows = store.UsageWindows
type cacheWindows = store.CacheWindows
type targetResolutionHealth = store.TargetHealth

// usage returns token totals per client key, virtual model, and real model for
// the last hour, last 24 hours, and last week. Read-only aggregation over
// request_logs; no cost/pricing.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	snap, err := s.buildUsageSnapshot(r.Context(), s.scope(r).AccountID())
	if err != nil {
		adminError(w, 500, "database_error", "Could not load usage.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"client_keys":         snap.ClientKeys,
		"virtual_models":      snap.VirtualModels,
		"target_health":       snap.TargetHealth,
		"target_cooldown":     snap.TargetCooldown,
		"real_models":         snap.RealModels,
		"client_cache":        snap.ClientCache,
		"virtual_cache":       snap.VirtualCache,
		"real_cache":          snap.RealCache,
		"target_last_outcome": snap.TargetLastOutcome,
	})
}

// usageAggregateTTL bounds reuse of the DB-derived usage aggregates. It is
// short enough that token counters stay effectively live, but long enough that
// the baseline SSE snapshot emitted on connect, the page's parallel
// /api/admin/usage request, and any other open admin tab share one set of
// request_logs scans instead of each paying for a full recompute.
const usageAggregateTTL = 2 * time.Second

// usageAggregates is the DB-derived portion of the usage snapshot (the
// request_logs aggregations). The live in-memory state — last outcomes,
// cooldowns, and in-flight requests — is deliberately excluded so it is never
// cached and always reflects the current instant.
type usageAggregates struct {
	TargetHealth  map[string]targetResolutionHealth
	VirtualModels map[string]usageWindows
	ClientKeys    map[string]usageWindows
	RealModels    map[string]usageWindows
	VirtualCache  map[string]cacheWindows
	ClientCache   map[string]cacheWindows
	RealCache     map[string]cacheWindows
}

// buildUsageSnapshot computes the full usage/health envelope shared by the
// /api/admin/usage endpoint and the live SSE snapshot event, so the two can
// never drift. It is the single source of truth for the aggregate recompute.
// The expensive DB-derived aggregates are reused for usageAggregateTTL via
// usageAggregates; the in-memory state is always read fresh.
func (s *Server) buildUsageSnapshot(ctx context.Context, accountID string) (liveSnapshot, error) {
	now := time.Now().UTC()
	agg, err := s.usageAggregates(ctx, accountID, now)
	if err != nil {
		return liveSnapshot{}, err
	}
	return liveSnapshot{
		GeneratedAt:       now.Format(time.RFC3339Nano),
		TargetLastOutcome: s.lastOutcomeSnapshot(accountID),
		TargetCooldown:    s.cooldown.snapshot(accountID, now),
		TargetHealth:      agg.TargetHealth,
		VirtualModels:     agg.VirtualModels,
		ClientKeys:        agg.ClientKeys,
		RealModels:        agg.RealModels,
		VirtualCache:      agg.VirtualCache,
		ClientCache:       agg.ClientCache,
		RealCache:         agg.RealCache,
		Modules: map[string]any{
			"inflight_clients":       s.inflight.clientSnapshot(accountID),
			"inflight_client_routes": s.inflight.clientRouteSnapshot(accountID),
			"inflight_targets":       s.inflight.targetSnapshot(accountID),
		},
	}, nil
}

// usageAggregates returns the DB-derived usage aggregates, reusing a recently
// computed set within usageCacheTTL. A zero TTL disables reuse. The mutex is
// held across the computation so concurrent callers coalesce onto one set of
// scans rather than stampeding the database.
func (s *Server) usageAggregates(ctx context.Context, accountID string, now time.Time) (usageAggregates, error) {
	s.usageAggMu.Lock()
	defer s.usageAggMu.Unlock()
	if s.usageCacheTTL > 0 {
		if cached := s.usageAgg[accountID]; cached != nil && now.Sub(s.usageAggAt[accountID]) < s.usageCacheTTL {
			return *cached, nil
		}
	}
	agg, err := s.computeUsageAggregates(ctx, accountID, now)
	if err != nil {
		return usageAggregates{}, err
	}
	if s.usageCacheTTL > 0 {
		s.usageAgg[accountID] = &agg
		s.usageAggAt[accountID] = now
	}
	return agg, nil
}

// invalidateUsageAggregates drops any cached aggregates so the next snapshot
// reflects a just-applied write (e.g. clearing or pruning activity). It is not
// needed for ordinary request logging, which the short TTL covers.
func (s *Server) invalidateUsageAggregates(accountID string) {
	s.usageAggMu.Lock()
	delete(s.usageAgg, accountID)
	delete(s.usageAggAt, accountID)
	s.usageAggMu.Unlock()
}

// invalidateAllUsageAggregates drops every account's cached aggregates. Used by
// the platform-wide retention pruner, which spans accounts.
func (s *Server) invalidateAllUsageAggregates() {
	s.usageAggMu.Lock()
	s.usageAgg = map[string]*usageAggregates{}
	s.usageAggAt = map[string]time.Time{}
	s.usageAggMu.Unlock()
}

// computeUsageAggregates runs the request_logs aggregation queries. It is the
// expensive path; callers should go through usageAggregates.
func (s *Server) computeUsageAggregates(ctx context.Context, accountID string, now time.Time) (usageAggregates, error) {
	cut1h := now.Add(-time.Hour).Format(time.RFC3339Nano)
	cut24h := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	cut7d := now.Add(-7 * 24 * time.Hour).Format(time.RFC3339Nano)
	sc := s.scopeFor(accountID)
	clientKeys, err := sc.UsageByClient(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	virtualModels, err := sc.UsageByVirtual(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	targetHealth, err := sc.TargetResolutionHealth(ctx, cut1h, cut24h)
	if err != nil {
		return usageAggregates{}, err
	}
	realModels, err := sc.UsageByReal(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	clientCache, err := sc.CacheByClient(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	virtualCache, err := sc.CacheByVirtual(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	realCache, err := sc.CacheByReal(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	return usageAggregates{
		TargetHealth:  targetHealth,
		VirtualModels: virtualModels,
		ClientKeys:    clientKeys,
		RealModels:    realModels,
		VirtualCache:  virtualCache,
		ClientCache:   clientCache,
		RealCache:     realCache,
	}, nil
}

// lastOutcomeSnapshot returns a copy of the in-memory per-real-model last
// request outcomes, keyed by "provider_name/upstream_model_id".
func (s *Server) lastOutcomeSnapshot(accountID string) map[string]lastOutcome {
	s.lastOutcomeMu.RLock()
	defer s.lastOutcomeMu.RUnlock()
	out := make(map[string]lastOutcome)
	prefix := accountID + "\x00"
	for k, v := range s.lastOutcome {
		if strings.HasPrefix(k, prefix) {
			out[k[len(prefix):]] = v
		}
	}
	return out
}
