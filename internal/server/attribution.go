package server

// This file is the single source of truth for activity attribution: "which
// request_logs rows belong to model X". List, CSV export, and usage all build
// their scoping from these helpers so no view can silently drift again.
//
// Attribution semantics:
//   - A real model owns current direct rows by stable route_model_id, virtual
//     rows by resolved provider/model, and legacy rows by resolved names.
//   - A virtual model owns rows routed to it (route_kind='virtual' AND
//     route_model_id), plus legacy rows that requested it by canonical name
//     (requested_model) or whose route_model is the canonical.

// virtualAttribution returns the SQL predicate + args that match rows
// attributable to a virtual model, handling both new rows (route_model_id) and
// legacy rows (route_kind NULL, matched by canonical name).
func virtualAttribution(virtualID, canonical string) (string, []any) {
	return `((route_kind='virtual' AND route_model_id=?) OR (route_status='legacy' AND route_kind IS NULL AND requested_model=?) OR (route_status='legacy' AND route_kind IS NULL AND route_model=?))`,
		[]any{virtualID, canonical, canonical}
}

// virtualAttributionJoin returns the SQL JOIN ON clause that matches a
// request_logs row (aliased l) to a virtual model row (aliased vm with id and
// canonical columns). It expresses the same attribution semantics as
// virtualAttribution so usage aggregation and activity can never drift.
func virtualAttributionJoin() string {
	return `(l.route_kind='virtual' AND l.route_model_id=vm.id) OR (l.route_status='legacy' AND l.route_kind IS NULL AND l.requested_model=vm.canonical) OR (l.route_status='legacy' AND l.route_kind IS NULL AND l.route_model=vm.canonical)`
}

// realAttribution returns the SQL predicate + args that match rows that
// resolved to a real model. Current rows use the stable route ID; names keep
// legacy rows and virtual-routed requests visible. A virtual row that never
// resolved (its fallback chain failed before any success) still belongs to
// every target it actually attempted, so failed request_attempts are matched
// too — otherwise the very request that opened a target's cooldown is missing
// from that target's activity. Callers must alias request_logs as "rl".
func realAttribution(modelID, provider, upstream string) (string, []any) {
	return `((rl.route_kind='real' AND rl.route_model_id=?) OR (rl.route_kind='virtual' AND rl.resolved_provider=? AND rl.resolved_model=?) OR (rl.route_status='legacy' AND rl.route_kind IS NULL AND rl.resolved_provider=? AND rl.resolved_model=?) OR (rl.route_kind='virtual' AND EXISTS (SELECT 1 FROM request_attempts ra WHERE ra.request_log_id=rl.id AND ra.provider=? AND ra.model=? AND ra.result='failed')))`, []any{modelID, provider, upstream, provider, upstream, provider, upstream}
}
