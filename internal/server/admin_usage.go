package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tiller-router/tiller-router/internal/store"
)

// usageWindows, cacheWindows and targetResolutionHealth are the store's
// account-scoped aggregate types; aliased here so the JSON envelope is
// unchanged.
type usageWindows = store.UsageWindows
type cacheWindows = store.CacheWindows
type costWindows = store.CostWindows
type tokenTypeWindows = store.TokenTypeWindows
type tokenSourceWindows = store.TokenSourceWindows
type targetResolutionHealth = store.TargetHealth

// usage returns token totals per client key, virtual model, and real model for
// the last hour, last 24 hours, and last week. Read-only aggregation over
// request_logs; no cost/pricing.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	snap, err := s.buildUsageSnapshot(r.Context(), s.scope(r).AccountID())
	if err != nil {
		// Usage is derived from Activity; an unavailable Activity store is the
		// same explicit degraded state, not a generic database failure.
		activityReadError(w, err, "Could not load usage.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"client_keys":           snap.ClientKeys,
		"virtual_models":        snap.VirtualModels,
		"target_health":         snap.TargetHealth,
		"target_cooldown":       snap.TargetCooldown,
		"real_models":           snap.RealModels,
		"client_cache":          snap.ClientCache,
		"virtual_cache":         snap.VirtualCache,
		"real_cache":            snap.RealCache,
		"client_cost":           snap.ClientCost,
		"virtual_cost":          snap.VirtualCost,
		"real_cost":             snap.RealCost,
		"client_tokens":         snap.ClientTokens,
		"virtual_tokens":        snap.VirtualTokens,
		"real_tokens":           snap.RealTokens,
		"client_token_sources":  snap.ClientTokenSources,
		"virtual_token_sources": snap.VirtualTokenSources,
		"real_token_sources":    snap.RealTokenSources,
		"target_last_outcome":   snap.TargetLastOutcome,
		"client_key_names":      snap.ClientKeyNames,
	})
}

// usageAggregateTTL is the floor for reuse of the DB-derived usage
// aggregates. It is short enough that token counters stay effectively live,
// but long enough that the baseline SSE snapshot emitted on connect, the
// page's parallel /api/admin/usage request, and any other open admin tab
// share one set of request_logs scans instead of each paying for a full
// recompute.
//
// The effective window is usually longer: it is derived from how long the
// last recompute actually took (see usageAggregateTTLFor), so an expensive
// scan is not repeated often enough to dominate the process.
const usageAggregateTTL = 2 * time.Second

// usageAggregateMaxTTL is the ceiling on the derived reuse window: however
// expensive a recompute was, the dashboard's counters never go more than this
// stale.
const usageAggregateMaxTTL = time.Minute

// usageAggregateBackoff is the duty-cycle target for the DB-derived
// aggregates. The reuse window is derived from the cost of the recompute that
// produced it, so the aggregate work occupies at most 1/N of the time
// regardless of how large the Activity table or the traffic becomes.
const usageAggregateBackoff = 20

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
	ClientCost    map[string]costWindows
	VirtualCost   map[string]costWindows
	RealCost      map[string]costWindows
	ClientTokens  map[string]tokenTypeWindows
	VirtualTokens map[string]tokenTypeWindows
	RealTokens    map[string]tokenTypeWindows
	// ClientTokenSources / VirtualTokenSources / RealTokenSources split each
	// group's input-token total by provenance (provider-reported vs locally
	// estimated). The headline totals above still include both; this exists so a
	// consumer can tell how much of a total was provider-reported.
	ClientTokenSources  map[string]tokenSourceWindows
	VirtualTokenSources map[string]tokenSourceWindows
	RealTokenSources    map[string]tokenSourceWindows
	// ClientKeyNames maps a client key id (the key space of every client-keyed
	// aggregate above) to its display name, so a consumer of the usage envelope
	// can label client rows without a second request. A usage id absent from
	// this map belongs to a deleted client key.
	ClientKeyNames map[string]string
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
		GeneratedAt:         now.Format(time.RFC3339Nano),
		TargetLastOutcome:   s.lastOutcomeSnapshot(accountID),
		TargetCooldown:      s.cooldown.snapshot(accountID, now),
		TargetHealth:        agg.TargetHealth,
		VirtualModels:       agg.VirtualModels,
		ClientKeys:          agg.ClientKeys,
		RealModels:          agg.RealModels,
		VirtualCache:        agg.VirtualCache,
		ClientCache:         agg.ClientCache,
		RealCache:           agg.RealCache,
		ClientCost:          agg.ClientCost,
		VirtualCost:         agg.VirtualCost,
		RealCost:            agg.RealCost,
		ClientTokens:        agg.ClientTokens,
		VirtualTokens:       agg.VirtualTokens,
		RealTokens:          agg.RealTokens,
		ClientTokenSources:  agg.ClientTokenSources,
		VirtualTokenSources: agg.VirtualTokenSources,
		RealTokenSources:    agg.RealTokenSources,
		ClientKeyNames:      agg.ClientKeyNames,
		Modules: map[string]any{
			"request_rows":           s.liveHub.requestSnapshot(accountID, false),
			"inflight_clients":       s.inflight.clientSnapshot(accountID),
			"inflight_client_routes": s.inflight.clientRouteSnapshot(accountID),
			"inflight_targets":       s.inflight.targetSnapshot(accountID),
		},
	}, nil
}

// usageAggEntry holds one account's cached aggregates. Its own mutex coalesces
// concurrent callers for that account, so one account's cold scan does not
// block another account's dashboard.
type usageAggEntry struct {
	mu  sync.Mutex
	agg *usageAggregates
	at  time.Time
	// ttl is the reuse window derived from the cost of the recompute that
	// produced agg. It is per account because the recompute cost is per
	// account.
	ttl time.Duration
}

// usageAggregateTTLFor derives the reuse window for a computed usage aggregate
// from the cost of the recompute that produced it:
//
//	window = max(floor, min(cost × usageAggregateBackoff, usageAggregateMaxTTL))
//
// Keying the window to the cost rather than to a fixed interval is what keeps
// the aggregate work bounded: the duty cycle is cost/window = 1/backoff, so it
// stays constant as the Activity table grows and as traffic rises. A fixed
// window instead degrades into a CPU hog exactly when the scans get expensive,
// which is when it matters most.
//
// floor is the minimum reuse window — an explicit override (a test's minute,
// say) always wins over the derived window, and is never shortened by it. A
// non-positive floor means reuse is disabled and the caller must not use the
// result.
func usageAggregateTTLFor(floor, cost time.Duration) time.Duration {
	if floor <= 0 {
		return floor
	}
	window := cost * usageAggregateBackoff
	if window > usageAggregateMaxTTL {
		window = usageAggregateMaxTTL
	}
	if window < floor {
		return floor
	}
	return window
}

// usageAggregates returns the DB-derived usage aggregates, reusing a recently
// computed set within the derived reuse window. A zero floor disables reuse.
// Callers for the same account coalesce onto one set of scans rather than
// stampeding the database.
func (s *Server) usageAggregates(ctx context.Context, accountID string, now time.Time) (usageAggregates, error) {
	entry := s.usageAggEntryFor(accountID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if s.usageCacheTTL > 0 && entry.agg != nil {
		if now.Sub(entry.at) < entry.ttl {
			return *entry.agg, nil
		}
		// Drop the stale set: otherwise every account that ever opened a
		// dashboard leaves its aggregates resident for the process life.
		entry.agg = nil
	}
	start := time.Now()
	agg, err := s.computeUsageAggregates(ctx, accountID, now)
	if err != nil {
		return usageAggregates{}, err
	}
	if s.usageCacheTTL > 0 {
		entry.agg, entry.at = &agg, now
		// The window is derived from the cost of the recompute just paid, so
		// the aggregate work keeps a bounded duty cycle as the Activity table
		// grows. See usageAggregateTTLFor.
		entry.ttl = usageAggregateTTLFor(s.usageCacheTTL, time.Since(start))
	}
	return agg, nil
}

// maxUsageAggAccounts bounds the per-account aggregate cache so a hosted
// instance with many historical accounts cannot grow it without limit. A
// dropped entry is simply recomputed on the next dashboard read.
const maxUsageAggAccounts = 256

// usageAggEntryFor returns the account's cache entry, creating it if needed and
// evicting an arbitrary entry when the cache is at its cap. Eviction is always
// safe because every entry is recomputed on demand.
func (s *Server) usageAggEntryFor(accountID string) *usageAggEntry {
	s.usageAggMu.Lock()
	defer s.usageAggMu.Unlock()
	if entry := s.usageAgg[accountID]; entry != nil {
		return entry
	}
	if len(s.usageAgg) >= maxUsageAggAccounts {
		for key := range s.usageAgg {
			delete(s.usageAgg, key)
			break
		}
	}
	entry := &usageAggEntry{}
	s.usageAgg[accountID] = entry
	return entry
}

// invalidateUsageAggregates drops any cached aggregates so the next snapshot
// reflects a just-applied write (e.g. clearing or pruning activity). It is not
// needed for ordinary request logging, which the short TTL covers.
func (s *Server) invalidateUsageAggregates(accountID string) {
	s.usageAggMu.Lock()
	entry := s.usageAgg[accountID]
	s.usageAggMu.Unlock()
	if entry == nil {
		return
	}
	entry.mu.Lock()
	entry.agg, entry.at = nil, time.Time{}
	entry.mu.Unlock()
}

// invalidateAllUsageAggregates drops every account's cached aggregates. Used by
// the platform-wide retention pruner, which spans accounts.
func (s *Server) invalidateAllUsageAggregates() {
	s.usageAggMu.Lock()
	s.usageAgg = map[string]*usageAggEntry{}
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
	clientCost, err := sc.CostByClient(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	virtualCost, err := sc.CostByVirtual(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	realCost, err := sc.CostByReal(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	clientTokens, err := sc.TokenTypesByClient(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	virtualTokens, err := sc.TokenTypesByVirtual(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	realTokens, err := sc.TokenTypesByReal(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	clientTokenSources, err := sc.TokenSourcesByClient(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	virtualTokenSources, err := sc.TokenSourcesByVirtual(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	realTokenSources, err := sc.TokenSourcesByReal(ctx, cut1h, cut24h, cut7d)
	if err != nil {
		return usageAggregates{}, err
	}
	clientKeyNames, err := sc.ClientKeyNames(ctx)
	if err != nil {
		return usageAggregates{}, err
	}
	return usageAggregates{
		TargetHealth:        targetHealth,
		VirtualModels:       virtualModels,
		ClientKeys:          clientKeys,
		RealModels:          realModels,
		VirtualCache:        virtualCache,
		ClientCache:         clientCache,
		RealCache:           realCache,
		ClientCost:          clientCost,
		VirtualCost:         virtualCost,
		RealCost:            realCost,
		ClientTokens:        clientTokens,
		VirtualTokens:       virtualTokens,
		RealTokens:          realTokens,
		ClientTokenSources:  clientTokenSources,
		VirtualTokenSources: virtualTokenSources,
		RealTokenSources:    realTokenSources,
		ClientKeyNames:      clientKeyNames,
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
