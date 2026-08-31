-- Backfill route attribution for legacy rows (route_kind NULL) where derivable.
--
-- Rows written before migration 010 populated route_kind/route_model_id/route_model
-- have these columns NULL. This migration derives them where possible:
--   - requested_model matches a virtual model canonical name -> route_kind='virtual'
--   - resolved_provider/resolved_model match a real model       -> route_kind='real'
-- Rows that match neither stay NULL (cannot be derived); they remain attributable
-- via resolved_* / requested_model. The migration is additive (only fills NULLs).

-- Virtual: rows whose requested_model matches a virtual model canonical.
UPDATE request_logs
SET route_kind='virtual',
    route_model_id=v.id,
    route_model=g.name||'/'||v.name
FROM virtual_models v
JOIN virtual_provider_groups g ON g.id=v.virtual_group_id
WHERE request_logs.route_kind IS NULL
  AND request_logs.requested_model = g.name||'/'||v.name;

-- Real: rows whose resolved names match a real model. Runs after the virtual
-- backfill so a virtual-routed row (already tagged 'virtual') is not re-tagged.
UPDATE request_logs
SET route_kind='real',
    route_model_id=m.id,
    route_model=p.name||'/'||m.upstream_model_id
FROM provider_models m
JOIN providers p ON p.id=m.provider_id
WHERE request_logs.route_kind IS NULL
  AND request_logs.resolved_provider = p.name
  AND request_logs.resolved_model = m.upstream_model_id;
