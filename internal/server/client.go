package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
)

const maxUpstreamNonStreamBytes int64 = 64 << 20

// maxUpstreamErrorBytes bounds how much of an upstream error response body we
// read for passthrough to the originating client. When detailed error
// logging is enabled, the bounded body is also retained on the activity row;
// otherwise it is written only to the client. Virtual fallback paths skip the
// read unless detailed error logging is on.
const maxUpstreamErrorBytes int64 = 1 << 20

var errUpstreamResponseTooLarge = errors.New("upstream response exceeds the non-streaming response limit")

func (s *Server) clientModels(w http.ResponseWriter, r *http.Request) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	var keyType string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT key_type FROM client_keys WHERE id=?`, identity.ID).Scan(&keyType); err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	anthropic := isAnthropicRequest(r)
	if keyType == "single" {
		var modelName, realID, virtualID string
		var real, virtual sql.NullString
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT exposed_model_name,real_model_id,virtual_model_id FROM client_single_bindings WHERE client_key_id=?`, identity.ID).Scan(&modelName, &real, &virtual); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				inferenceError(w, 500, "server_error", "invalid_single_binding", "No binding configured for this API key.", false)
			} else {
				inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
			}
			return
		}
		realID, virtualID = real.String, virtual.String
		var contextLength, maxOutputTokens sql.NullInt64
		var caps modelCapabilities
		var reasoningCaps *providers.ReasoningCapabilities
		if real.Valid {
			var reasoningRaw sql.NullString
			if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT context_length,max_output_tokens,supports_tools,supports_vision,supports_reasoning,supports_structured_output,reasoning_capabilities FROM provider_models WHERE id=?`, realID).Scan(&contextLength, &maxOutputTokens, &caps.Tools, &caps.Vision, &caps.Reasoning, &caps.StructuredOutput, &reasoningRaw); err != nil {
				inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
				return
			}
			reasoningCaps = decodeReasoningCapabilities(reasoningRaw)
		} else {
			aggregated, err := s.loadVirtualCapabilities(r.Context(), []string{virtualID})
			if err != nil {
				inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
				return
			}
			contextLength, maxOutputTokens, caps, reasoningCaps = catalogueCapabilityFields(aggregated[virtualID])
		}
		entry := buildCatalogueEntry(modelName, contextLength, maxOutputTokens, caps, reasoningCaps, anthropic)
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{entry}})
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT canonical, context_length, max_output_tokens, supports_tools, supports_vision, supports_reasoning, supports_structured_output, reasoning_capabilities, virtual_model_id FROM (
	SELECT p.name||'/'||m.upstream_model_id canonical, m.context_length, m.max_output_tokens, m.supports_tools, m.supports_vision, m.supports_reasoning, m.supports_structured_output, m.reasoning_capabilities, NULL virtual_model_id FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND m.available=1 AND p.enabled=1
	UNION ALL
	SELECT g.name||'/'||v.name canonical, NULL, NULL, NULL, NULL, NULL, NULL, NULL, v.id virtual_model_id FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE x.client_key_id=? AND x.enabled=1 AND EXISTS(SELECT 1 FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=v.id AND t.enabled=1 AND m.available=1 AND p.enabled=1)
	) ORDER BY canonical`, identity.ID, identity.ID)
	if err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	type catalogueRow struct {
		modelID                        string
		contextLength, maxOutputTokens sql.NullInt64
		caps                           modelCapabilities
		reasoningRaw, virtualID        sql.NullString
	}
	catalogueRows := []catalogueRow{}
	for rows.Next() {
		var row catalogueRow
		if err := rows.Scan(&row.modelID, &row.contextLength, &row.maxOutputTokens, &row.caps.Tools, &row.caps.Vision, &row.caps.Reasoning, &row.caps.StructuredOutput, &row.reasoningRaw, &row.virtualID); err != nil {
			rows.Close()
			inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
			return
		}
		catalogueRows = append(catalogueRows, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	if err := rows.Close(); err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	virtualIDs := make([]string, 0)
	for _, row := range catalogueRows {
		if row.virtualID.Valid {
			virtualIDs = append(virtualIDs, row.virtualID.String)
		}
	}
	aggregated, err := s.loadVirtualCapabilities(r.Context(), virtualIDs)
	if err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	data := []map[string]any{}
	for _, row := range catalogueRows {
		reasoningCaps := decodeReasoningCapabilities(row.reasoningRaw)
		if row.virtualID.Valid {
			row.contextLength, row.maxOutputTokens, row.caps, reasoningCaps = catalogueCapabilityFields(aggregated[row.virtualID.String])
		}
		data = append(data, buildCatalogueEntry(row.modelID, row.contextLength, row.maxOutputTokens, row.caps, reasoningCaps, anthropic))
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// clientModel handles model-detail lookups. A Single key has one configured
// route, so the requested path is intentionally ignored just like it is for
// inference requests.
func (s *Server) clientModel(w http.ResponseWriter, r *http.Request) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	var keyType string
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT key_type FROM client_keys WHERE id=?`, identity.ID).Scan(&keyType); err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model metadata.", false)
		return
	}
	if keyType != "single" {
		inferenceError(w, 404, "invalid_request_error", "model_not_found", "Model not found.", false)
		return
	}
	var modelName, realID, virtualID string
	var real, virtual sql.NullString
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT exposed_model_name,real_model_id,virtual_model_id FROM client_single_bindings WHERE client_key_id=?`, identity.ID).Scan(&modelName, &real, &virtual); err != nil {
		inferenceError(w, 500, "server_error", "invalid_single_binding", "Could not load the Single model binding.", false)
		return
	}
	realID, virtualID = real.String, virtual.String
	var contextLength, maxOutputTokens sql.NullInt64
	var caps modelCapabilities
	if real.Valid {
		var reasoningRaw sql.NullString
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT context_length,max_output_tokens,supports_tools,supports_vision,supports_reasoning,supports_structured_output,reasoning_capabilities FROM provider_models WHERE id=?`, realID).Scan(&contextLength, &maxOutputTokens, &caps.Tools, &caps.Vision, &caps.Reasoning, &caps.StructuredOutput, &reasoningRaw); err != nil {
			inferenceError(w, 500, "server_error", "invalid_single_binding", "Could not load the Single model metadata.", false)
			return
		}
		entry := buildCatalogueEntry(modelName, contextLength, maxOutputTokens, caps, decodeReasoningCapabilities(reasoningRaw), isAnthropicRequest(r))
		writeJSON(w, 200, entry)
		return
	}
	aggregated, err := s.loadVirtualCapabilities(r.Context(), []string{virtualID})
	if err != nil {
		inferenceError(w, 500, "server_error", "invalid_single_binding", "Could not load the Single model metadata.", false)
		return
	}
	contextLength, maxOutputTokens, caps, reasoningCaps := catalogueCapabilityFields(aggregated[virtualID])
	entry := buildCatalogueEntry(modelName, contextLength, maxOutputTokens, caps, reasoningCaps, isAnthropicRequest(r))
	writeJSON(w, 200, entry)
}

// isAnthropicRequest returns true when the request carries an Anthropic
// version header, signalling that the client expects Anthropic capability
// metadata in the Tiller catalogue.
func isAnthropicRequest(r *http.Request) bool {
	return r.Header.Get("anthropic-version") != ""
}

// addReasoningToCatalogueEntry publishes the normalized reasoning metadata in
// the catalogue shapes understood by OpenAI-compatible and Anthropic clients.
func addReasoningToCatalogueEntry(entry map[string]any, rc *providers.ReasoningCapabilities, anthropic bool) {
	if rc == nil {
		return
	}
	if anthropic {
		caps := map[string]any{}
		var thinking map[string]any
		for _, opt := range rc.Options {
			switch opt.Type {
			case providers.ReasoningOptionEffort:
				effort := map[string]any{"supported": true}
				for _, level := range providers.CanonicalEffortOrder() {
					for _, value := range opt.Values {
						if value == level {
							effort[level] = map[string]any{"supported": true}
							break
						}
					}
				}
				caps["effort"] = effort
			case providers.ReasoningOptionToggle:
				thinking = map[string]any{"supported": true}
			}
		}
		if len(rc.ThinkingModes) > 0 {
			if thinking == nil {
				thinking = map[string]any{"supported": true}
			}
			types := map[string]any{}
			for _, mode := range rc.ThinkingModes {
				switch mode {
				case "adaptive", "enabled":
					types[mode] = map[string]any{"supported": true}
				}
			}
			if len(types) > 0 {
				thinking["types"] = types
			}
		}
		if thinking != nil {
			caps["thinking"] = thinking
		}
		if len(caps) > 0 {
			entry["capabilities"] = caps
		}
		return
	}

	var effortValues []string
	var hasEffort bool
	var options []map[string]any
	for _, opt := range rc.Options {
		switch opt.Type {
		case providers.ReasoningOptionEffort:
			hasEffort = true
			effortValues = opt.Values
			options = append(options, map[string]any{"type": "effort", "values": opt.Values})
		case providers.ReasoningOptionToggle:
			options = append(options, map[string]any{"type": "toggle"})
		case providers.ReasoningOptionBudgetTokens:
			budget := map[string]any{"type": "budget_tokens"}
			if opt.Min != nil {
				budget["min"] = *opt.Min
			}
			if opt.Max != nil {
				budget["max"] = *opt.Max
			}
			options = append(options, budget)
		}
	}
	if len(options) > 0 {
		entry["reasoning_options"] = options
	}
	if hasEffort {
		entry["reasoning"] = map[string]any{"supported_efforts": effortValues}
	}
}

func (s *Server) loadVirtualCapabilities(ctx context.Context, virtualModelIDs []string) (map[string]aggregatedVirtualCapabilities, error) {
	result := make(map[string]aggregatedVirtualCapabilities, len(virtualModelIDs))
	if len(virtualModelIDs) == 0 {
		return result, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(virtualModelIDs)), ",")
	rows, err := s.db.SQL.QueryContext(ctx, `SELECT t.virtual_model_id,m.context_length,m.max_output_tokens,m.supports_tools,m.supports_vision,m.supports_reasoning,m.supports_structured_output,m.reasoning_capabilities FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id IN (`+placeholders+`) AND t.enabled=1 AND m.available=1 AND p.enabled=1`, stringSliceToAny(virtualModelIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make(map[string][]virtualTargetCapabilities, len(virtualModelIDs))
	for rows.Next() {
		var virtualID string
		var contextLength, maxOutputTokens, tools, vision, reasoning, structured sql.NullInt64
		var reasoningRaw sql.NullString
		if err := rows.Scan(&virtualID, &contextLength, &maxOutputTokens, &tools, &vision, &reasoning, &structured, &reasoningRaw); err != nil {
			return nil, err
		}
		targets[virtualID] = append(targets[virtualID], virtualTargetCapabilities{
			ContextLength: nullInt64Ptr(contextLength), MaxOutputTokens: nullInt64Ptr(maxOutputTokens),
			SupportsTools: triBoolFromInt(tools), SupportsVision: triBoolFromInt(vision),
			SupportsReasoning: triBoolFromInt(reasoning), SupportsStructuredOutput: triBoolFromInt(structured),
			ReasoningCapabilities: decodeReasoningCapabilities(reasoningRaw),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, virtualID := range virtualModelIDs {
		result[virtualID] = aggregateVirtualCapabilities(targets[virtualID])
	}
	return result, nil
}

func nullInt64Ptr(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func catalogueCapabilityFields(aggregated aggregatedVirtualCapabilities) (sql.NullInt64, sql.NullInt64, modelCapabilities, *providers.ReasoningCapabilities) {
	toNullInt := func(value *int64) sql.NullInt64 {
		if value == nil {
			return sql.NullInt64{}
		}
		return sql.NullInt64{Int64: *value, Valid: true}
	}
	toCapability := func(value *bool) sql.NullInt64 {
		if value == nil {
			return sql.NullInt64{}
		}
		if *value {
			return sql.NullInt64{Int64: 1, Valid: true}
		}
		return sql.NullInt64{Int64: 0, Valid: true}
	}
	return toNullInt(aggregated.ContextLength), toNullInt(aggregated.MaxOutputTokens), modelCapabilities{
		Tools: toCapability(aggregated.SupportsTools), Vision: toCapability(aggregated.SupportsVision),
		Reasoning: toCapability(aggregated.SupportsReasoning), StructuredOutput: toCapability(aggregated.SupportsStructuredOutput),
	}, aggregated.ReasoningCapabilities
}

func stringSliceToAny(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func buildCatalogueEntry(modelID string, contextLength, maxOutputTokens sql.NullInt64, caps modelCapabilities, reasoningCaps *providers.ReasoningCapabilities, anthropic bool) map[string]any {
	entry := map[string]any{"id": modelID, "object": "model", "created": 0, "owned_by": "tiller-router"}
	if contextLength.Valid && contextLength.Int64 > 0 {
		entry["context_length"] = contextLength.Int64
	}
	if maxOutputTokens.Valid && maxOutputTokens.Int64 > 0 {
		entry["max_output_tokens"] = maxOutputTokens.Int64
	}
	caps.addTo(entry)
	addReasoningToCatalogueEntry(entry, reasoningCaps, anthropic)
	return entry
}

// modelCapabilities holds the tri-state capability flags for a model. A flag is
// Valid only when the provider reported it; unknown flags are omitted from the
// client-facing catalogue rather than being reported as unsupported.
type modelCapabilities struct {
	Tools, Vision, Reasoning, StructuredOutput sql.NullInt64
}

func (c modelCapabilities) addTo(entry map[string]any) {
	if c.Tools.Valid {
		entry["supports_tools"] = c.Tools.Int64
	}
	if c.Vision.Valid {
		entry["supports_vision"] = c.Vision.Int64
	}
	if c.Reasoning.Valid {
		entry["supports_reasoning"] = c.Reasoning.Int64
	}
	if c.StructuredOutput.Valid {
		entry["supports_structured_output"] = c.StructuredOutput.Int64
	}
}

type resolvedRoute struct {
	Provider                            providers.Instance
	ProviderModelID                     string
	UpstreamModelID, RequestedModel     string
	NativeProtocol                      providers.Protocol
	Virtual, Available                  bool
	RoutingMode                         string
	Targets                             []resolvedRoute
	RouteKind, RouteModelID, RouteModel string
	// ReasoningCapabilities holds the normalized selector metadata for this
	// real target. nil when unknown.
	ReasoningCapabilities *providers.ReasoningCapabilities
	MaxOutputTokens       sql.NullInt64
}

func (s *Server) resolveRoute(ctx context.Context, clientID, requested string) (resolvedRoute, error) {
	tx, err := s.db.SQL.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return resolvedRoute{}, err
	}
	defer tx.Rollback()
	var keyType string
	if err = tx.QueryRowContext(ctx, `SELECT key_type FROM client_keys WHERE id=?`, clientID).Scan(&keyType); err != nil {
		return resolvedRoute{}, err
	}
	var route resolvedRoute
	clientModel := requested
	if keyType == "single" {
		var realID, virtualID sql.NullString
		if err = tx.QueryRowContext(ctx, `SELECT exposed_model_name,real_model_id,virtual_model_id FROM client_single_bindings WHERE client_key_id=?`, clientID).Scan(&clientModel, &realID, &virtualID); err != nil {
			return resolvedRoute{}, err
		}
		if realID.Valid {
			route.RouteKind, route.RouteModelID = "real", realID.String
			route.ProviderModelID = realID.String
			var reasoningRaw sql.NullString
			if err = tx.QueryRowContext(ctx, `SELECT p.name||'/'||m.upstream_model_id, m.reasoning_capabilities, m.max_output_tokens FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?`, realID.String).Scan(&route.RouteModel, &reasoningRaw, &route.MaxOutputTokens); err != nil {
				return resolvedRoute{}, err
			}
			route.ReasoningCapabilities = decodeReasoningCapabilities(reasoningRaw)
		} else {
			route.RouteKind, route.RouteModelID = "virtual", virtualID.String
			if err = tx.QueryRowContext(ctx, `SELECT g.name||'/'||v.name FROM virtual_models v JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE v.id=?`, virtualID.String).Scan(&route.RouteModel); err != nil {
				return resolvedRoute{}, err
			}
		}
	} else {
		var reasoningRaw sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT m.id,p.name||'/'||m.upstream_model_id, m.reasoning_capabilities, m.max_output_tokens FROM client_model_permissions x JOIN provider_models m ON x.model_kind='real' AND x.model_id=m.id JOIN providers p ON p.id=m.provider_id WHERE x.client_key_id=? AND x.enabled=1 AND p.name||'/'||m.upstream_model_id=?`, clientID, requested).Scan(&route.RouteModelID, &route.RouteModel, &reasoningRaw, &route.MaxOutputTokens)
		if err == nil {
			route.RouteKind = "real"
			route.ReasoningCapabilities = decodeReasoningCapabilities(reasoningRaw)
		} else if err == sql.ErrNoRows {
			err = tx.QueryRowContext(ctx, `SELECT v.id,g.name||'/'||v.name FROM client_model_permissions x JOIN virtual_models v ON x.model_kind='virtual' AND x.model_id=v.id JOIN virtual_provider_groups g ON g.id=v.virtual_group_id WHERE x.client_key_id=? AND x.enabled=1 AND g.name||'/'||v.name=?`, clientID, requested).Scan(&route.RouteModelID, &route.RouteModel)
			if err != nil {
				return resolvedRoute{}, err
			}
			route.RouteKind = "virtual"
		} else {
			return resolvedRoute{}, err
		}
	}
	if route.RouteKind == "virtual" {
		// Capture the routing mode so cooldown applies only to ordered
		// fallback virtual models (fixed virtual routes and direct real-model
		// routes must never populate or consult the shared cooldown state).
		if err := tx.QueryRowContext(ctx, `SELECT routing_mode FROM virtual_models WHERE id=?`, route.RouteModelID).Scan(&route.RoutingMode); err != nil {
			return resolvedRoute{}, err
		}
		rows, e := tx.QueryContext(ctx, `SELECT m.id,p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available,m.reasoning_capabilities,m.max_output_tokens FROM virtual_model_targets t JOIN provider_models m ON m.id=t.provider_model_id JOIN providers p ON p.id=m.provider_id WHERE t.virtual_model_id=? AND t.enabled=1 ORDER BY t.position`, route.RouteModelID)
		if e != nil {
			return resolvedRoute{}, e
		}
		defer rows.Close()
		for rows.Next() {
			var target resolvedRoute
			var protocols string
			var enabled, available int
			var nativeProtocol sql.NullString
			var reasoningRaw sql.NullString
			if e = rows.Scan(&target.ProviderModelID, &target.Provider.ID, &target.Provider.Name, &target.Provider.Type, &target.Provider.BaseURL, &target.Provider.Credential, &enabled, &protocols, &nativeProtocol, &target.UpstreamModelID, &available, &reasoningRaw, &target.MaxOutputTokens); e != nil {
				return resolvedRoute{}, e
			}
			target.Provider.Enabled = scanBool(enabled)
			target.Provider.Protocols = providers.DecodeProtocols(protocols)
			if d, ok := providers.Lookup(target.Provider.Type); ok {
				target.Provider.MinOutputTokens = d.MinOutputTokens
			}
			// OAuth hydration happens after commit (see below) so the
			// SQLite read tx is never held across network refresh calls.
			if nativeProtocol.Valid {
				target.NativeProtocol = providers.Protocol(nativeProtocol.String)
			}
			target.ReasoningCapabilities = decodeReasoningCapabilities(reasoningRaw)
			target.Available = target.Provider.Enabled && scanBool(available)
			target.Virtual, target.RequestedModel = true, clientModel
			target.RouteKind, target.RouteModelID, target.RouteModel = route.RouteKind, route.RouteModelID, route.RouteModel
			route.Targets = append(route.Targets, target)
		}
		if e = rows.Err(); e != nil {
			return resolvedRoute{}, e
		}
		if err := tx.Commit(); err != nil {
			return resolvedRoute{}, err
		}
		// Hydrate OAuth credentials outside the transaction: Current() can
		// trigger a network token refresh, and holding a SQLite read tx
		// across that serializes all other DB access.
		for i := range route.Targets {
			s.providers.HydrateOAuth(ctx, &route.Targets[i].Provider)
		}
		route.Virtual, route.RequestedModel = true, clientModel
		if len(route.Targets) > 0 {
			route.Provider, route.UpstreamModelID, route.Available = route.Targets[0].Provider, route.Targets[0].UpstreamModelID, route.Targets[0].Available
		}
		return route, nil
	}
	var protocols string
	var enabled, modelAvailable int
	var nativeProtocol sql.NullString
	var reasoningRaw sql.NullString
	route.ProviderModelID = route.RouteModelID
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.name,p.type,p.base_url,coalesce(p.credential_secret,''),p.enabled,p.protocols,m.native_protocol,m.upstream_model_id,m.available,m.reasoning_capabilities FROM provider_models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?`, route.RouteModelID).Scan(&route.Provider.ID, &route.Provider.Name, &route.Provider.Type, &route.Provider.BaseURL, &route.Provider.Credential, &enabled, &protocols, &nativeProtocol, &route.UpstreamModelID, &modelAvailable, &reasoningRaw)
	if err != nil {
		return resolvedRoute{}, err
	}
	route.ReasoningCapabilities = decodeReasoningCapabilities(reasoningRaw)
	route.Provider.Enabled = scanBool(enabled)
	route.Provider.Protocols = providers.DecodeProtocols(protocols)
	if d, ok := providers.Lookup(route.Provider.Type); ok {
		route.Provider.MinOutputTokens = d.MinOutputTokens
	}
	if nativeProtocol.Valid {
		route.NativeProtocol = providers.Protocol(nativeProtocol.String)
	}
	route.RequestedModel = clientModel
	route.Virtual = false
	route.Available = route.Provider.Enabled && scanBool(modelAvailable)
	if err := tx.Commit(); err != nil {
		return resolvedRoute{}, err
	}
	// OAuth hydration outside the transaction (see virtual branch above).
	s.providers.HydrateOAuth(ctx, &route.Provider)
	return route, nil
}

// idleTimeout is how long a successful upstream response may sit silent
// before the router cancels it. It is the idleReader's reset interval and
// the AfterFunc's initial deadline.
const idleTimeout = 5 * time.Minute

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, incoming providers.Protocol) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		inferenceError(w, 400, "invalid_request_error", "request_too_large", "Request JSON exceeds the 32 MiB limit.", incoming == providers.ProtocolMessages)
		return
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		inferenceError(w, 400, "invalid_request_error", "invalid_json", "Request body must be valid JSON.", incoming == providers.ProtocolMessages)
		return
	}
	var requested string
	if json.Unmarshal(raw["model"], &requested) != nil || requested == "" {
		inferenceError(w, 400, "invalid_request_error", "model_required", "A model identifier is required.", incoming == providers.ProtocolMessages)
		return
	}
	// Begin request logging once a valid client + model is present. The row is
	// built up as the request progresses and written once, synchronously, in a
	// deferred best-effort insert that never fails the request.
	row := &logRow{
		clientKeyID:     identity.ID,
		clientName:      identity.Name,
		requestedModel:  requested,
		routeStatus:     "unresolved",
		protocol:        string(incoming),
		clientRequestID: newRequestID(),
		createdAt:       database.Now(),
	}
	logErrorBodies, _ := s.db.GetLogErrorBodies(r.Context())
	originalBody := append([]byte(nil), body...)
	// Extract the canonical reasoning selector once from the original request.
	// It is recomputed for each candidate against that target's capabilities.
	canonicalSelector := extractReasoningSelector(originalBody, incoming)
	start := time.Now()
	streamed := false
	clientTracked := false
	activeTargetID := ""
	var route resolvedRoute
	defer func() {
		row.latencyMs = time.Since(start).Milliseconds()
		if logErrorBodies && row.httpStatus >= 400 {
			row.requestBody, row.requestBodyTruncated = loggedBody(originalBody)
		}
		if route.Virtual {
			s.inflight.end(route.RouteModelID, streamed)
		}
		if clientTracked {
			s.inflight.clientEnd(row.clientKeyID, streamed)
		}
		if activeTargetID != "" {
			s.inflight.targetEnd(route.RouteModelID, activeTargetID)
		}
		s.writeLog(context.Background(), row)
	}()
	w.Header().Set("X-Tiller-Request-Id", row.clientRequestID)

	route, err = s.resolveRoute(r.Context(), identity.ID, requested)
	if err == sql.ErrNoRows {
		row.httpStatus = 404
		row.errorText = strPtr("model_not_found")
		row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("model_not_found"))
		inferenceError(w, 404, "invalid_request_error", "model_not_found", "Model not found.", incoming == providers.ProtocolMessages)
		return
	} else if err != nil {
		if s.logger != nil {
			s.logger.Warn("resolveRoute failed", "client_request_id", row.clientRequestID, "requested_model", requested, "error", err.Error())
		}
		row.httpStatus = 500
		row.errorText = strPtr("database_error")
		row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("database_error"))
		inferenceError(w, 500, "server_error", "database_error", "Could not resolve the model.", incoming == providers.ProtocolMessages)
		return
	}
	row.exposedModel = &route.RequestedModel
	row.routeStatus = "routed"
	row.routeKind = &route.RouteKind
	row.routeModelID = &route.RouteModelID
	row.routeModel = &route.RouteModel
	s.inflight.clientStart(row.clientKeyID, requested)
	clientTracked = true
	if route.Virtual {
		s.inflight.start(route.RouteModelID)
	}
	candidates := []resolvedRoute{route}
	if route.Virtual {
		candidates = route.Targets
	}
	var resp *http.Response
	var selected resolvedRoute
	var target providers.Protocol
	var idle *time.Timer
	var attemptTimedOut *atomic.Bool
	var translated bool
	var cancel context.CancelFunc
	protocolUnavailable := false
	translationFailureClass := ""
	nonTranslationFailure := false
	terminalPreflightClass := ""
	oauthRefreshed := make(map[string]bool)
	cooldownSeconds := 0
	if route.RoutingMode == "ordered_fallback" {
		cooldownSeconds, _ = s.db.GetFallbackCooldownSeconds(r.Context())
	}
	skippedCooled := false
	allAttemptedFailed := true
	var success bool
	for pass := 0; pass < 2 && !success; pass++ {
		bypass := pass == 1
		if bypass && (!skippedCooled || !allAttemptedFailed) {
			break
		}
		for i := 0; i < len(candidates); i++ {
			candidate := candidates[i]
			if ctxErr := r.Context().Err(); ctxErr != nil {
				class := "client_cancelled"
				if errors.Is(ctxErr, context.DeadlineExceeded) {
					class = "client_timeout"
				}
				row.httpStatus = 502
				row.errorText = strPtr(class)
				row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
				row.fallbackReason = strPtr(class)
				inferenceError(w, 502, "api_error", class, "The client request ended before fallback could complete.", incoming == providers.ProtocolMessages)
				return
			}
			attemptStart := time.Now()
			if !candidate.Available {
				nonTranslationFailure = true
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "unavailable"})
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				continue
			}
			if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && !bypass && s.cooldown.cooled(candidate.ProviderModelID, attemptStart) {
				skippedCooled = true
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "cooldown", latencyMs: time.Since(attemptStart).Milliseconds()})
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				continue
			}
			if candidate.Provider.Credential != "" && (candidate.Provider.Type == "opencode-zen" || candidate.Provider.Type == "opencode-go") && providers.IsOpenCodeFreeModel(candidate.UpstreamModelID) {
				// Free-tier models are served anonymously on the Zen relay: any
				// unrecognized bearer is rejected with 401, so a keyed
				// zen/go instance can never serve them. Fail loud with a
				// remediation instead of burning the attempt upstream — never
				// silently re-route to another provider.
				if !route.Virtual {
					row.httpStatus = 400
					row.errorText = strPtr("free_model_requires_keyless")
					row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("free_model_requires_keyless"))
					inferenceError(w, 400, "invalid_request_error", "free_model_requires_keyless", "This is an OpenCode free-tier model and cannot be served with a credential. Configure it through an opencode-free provider instead.", incoming == providers.ProtocolMessages)
					return
				}
				nonTranslationFailure = true
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "free_model_requires_keyless", errorMessage: strPtrIfNonEmpty(fixedUpstreamErrorMessage("free_model_requires_keyless")), latencyMs: time.Since(attemptStart).Milliseconds()})
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				continue
			}
			target = compatibleProtocol(candidate.Provider.Protocols, candidate.NativeProtocol, incoming)
			if target == "" {
				protocolUnavailable = true
				nonTranslationFailure = true
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "protocol_unavailable"})
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				continue
			}
			translated = target != incoming
			attemptBody := append([]byte(nil), originalBody...)
			if translated {
				attemptBody, err = translateRequest(attemptBody, incoming, target, candidate.UpstreamModelID, candidate.MaxOutputTokens.Int64)
				if err != nil {
					code := "translation_error"
					var unsupported unsupportedFeature
					if errors.As(err, &unsupported) {
						code = "unsupported_feature"
					}
					if !route.Virtual {
						row.httpStatus = 400
						row.errorText = strPtr(code)
						row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(code))
						inferenceError(w, 400, "invalid_request_error", code, err.Error(), incoming == providers.ProtocolMessages)
						return
					}
					translationFailureClass = code
					row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: code, errorMessage: strPtr(err.Error()), latencyMs: time.Since(attemptStart).Milliseconds()})
					continue
				}
				// After translation, re-apply the canonical selector for the target.
				attemptBody = applyReasoningSelector(attemptBody, canonicalSelector, target, candidate.ReasoningCapabilities)
			} else {
				var attemptRaw map[string]json.RawMessage
				_ = json.Unmarshal(attemptBody, &attemptRaw)
				attemptRaw["model"], _ = json.Marshal(candidate.UpstreamModelID)
				attemptBody, _ = json.Marshal(attemptRaw)
				// B3: plain-chat default-disable. A Chat client that sent no
				// reasoning selector gets an explicit disable when the Chat
				// target advertises one, so a reasoning-default upstream cannot
				// return a reasoning-only response with empty content.
				// Mandatory-reasoning targets cannot serve plain chat: skip on
				// virtual routes (fallback), fail loud on direct routes.
				if !canonicalSelector.Present && incoming == providers.ProtocolChat && target == providers.ProtocolChat {
					if isMandatoryReasoning(candidate.ReasoningCapabilities) {
						if !route.Virtual {
							row.httpStatus = 400
							row.errorText = strPtr("unsupported_feature")
							row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("unsupported_feature"))
							inferenceError(w, 400, "invalid_request_error", "unsupported_feature", "The model requires reasoning and cannot serve a plain non-reasoning request.", incoming == providers.ProtocolMessages)
							return
						}
						row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "unsupported_feature", errorMessage: strPtrIfNonEmpty(fixedUpstreamErrorMessage("unsupported_feature")), latencyMs: time.Since(attemptStart).Milliseconds()})
						continue
					}
					if disabled, ok := injectChatDisable(attemptBody, candidate.ReasoningCapabilities); ok {
						attemptBody = disabled
					}
				}
				// B3a: an explicit selector on a same-protocol Chat target is
				// validated against that target's capabilities, matching the
				// translated path. Unknown capabilities (caps==nil) are never
				// assumed to accept an effort value: the selector is stripped
				// so the provider default applies instead of a possible 400.
				if canonicalSelector.Present && incoming == providers.ProtocolChat && target == providers.ProtocolChat {
					if candidate.ReasoningCapabilities == nil {
						attemptBody = stripReasoningSelector(attemptBody, target)
					} else {
						attemptBody = applyReasoningSelector(attemptBody, canonicalSelector, target, candidate.ReasoningCapabilities)
					}
				}
			}
			if candidate.Provider.Type == "codex-subscription" {
				attemptBody, err = normalizeCodexRequest(attemptBody)
				if err != nil {
					row.httpStatus = 400
					row.errorText = strPtr("invalid_request")
					row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("invalid_request"))
					inferenceError(w, 400, "invalid_request_error", "invalid_request", "The Codex request could not be normalized.", incoming == providers.ProtocolMessages)
					return
				}
			}
			if minOut := candidate.Provider.MinOutputTokens; minOut > 0 {
				var compatible bool
				attemptBody, compatible, err = checkMinOutputTokens(attemptBody, minOut, target)
				if err != nil {
					row.httpStatus = 400
					row.errorText = strPtr("invalid_request")
					row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("invalid_request"))
					inferenceError(w, 400, "invalid_request_error", "invalid_request", "Could not apply minimum output tokens.", incoming == providers.ProtocolMessages)
					return
				}
				if !compatible {
					// The client explicitly requested fewer output tokens than
					// this target's provider minimum. Never silently raise an
					// explicit client limit: skip the target on virtual routes,
					// fail loud on direct routes.
					if !route.Virtual {
						row.httpStatus = 400
						row.errorText = strPtr("unsupported_feature")
						row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("unsupported_feature"))
						inferenceError(w, 400, "invalid_request_error", "unsupported_feature", "The model requires a higher minimum output length than requested.", incoming == providers.ProtocolMessages)
						return
					}
					row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "unsupported_feature", errorMessage: strPtrIfNonEmpty(fixedUpstreamErrorMessage("unsupported_feature")), latencyMs: time.Since(attemptStart).Milliseconds()})
					continue
				}
			}
			endpoint, e := providers.Endpoint(candidate.Provider, target)
			if e != nil {
				nonTranslationFailure = true
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: 0, failureClass: "invalid_upstream", latencyMs: time.Since(attemptStart).Milliseconds()})
				continue
			}
			upstreamCtx, attemptCancel := context.WithCancel(r.Context())
			req, e := http.NewRequestWithContext(upstreamCtx, http.MethodPost, endpoint, bytes.NewReader(attemptBody))
			if e != nil {
				attemptCancel()
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("User-Agent", "Tiller-Router/1")
			if candidate.Provider.Type == "opencode-free" {
				if clientIP := s.requestClientIP(r); clientIP != "" {
					req.Header.Set("X-Real-IP", clientIP)
				}
			}
			if candidate.Provider.Type == "opencode-zen" || candidate.Provider.Type == "opencode-go" || candidate.Provider.Type == "opencode-free" {
				// OpenCode requires a stable per-conversation session ID for
				// routing/prompt-caching (MissingSessionID 400 otherwise).
				// Forward a native client header when present so OpenCode,
				// Hermes, etc. keep conversation affinity; otherwise
				// synthesize one from the router request ID (stable across
				// fallback attempts of the same client request).
				session := openCodeSessionID(r.Header.Get("x-opencode-session"), row.clientRequestID, row.clientKeyID)
				req.Header.Set("X-Opencode-Session", session)
				req.Header.Set("X-Opencode-Client", "tiller-router")
			}
			providers.ApplyRequestAuth(req, candidate.Provider)
			copySafeFeatureHeaders(req.Header, r.Header, target)
			if candidate.Provider.Type == "codex-subscription" {
				req.Header.Set("session-id", row.clientRequestID)
			}
			targetID := candidate.ProviderModelID
			if targetID == "" {
				targetID = candidate.Provider.Name + "/" + candidate.UpstreamModelID
			}
			if route.Virtual {
				s.inflight.targetStart(route.RouteModelID, targetID)
			}
			response, e := s.providers.Registry().HTTPClient().Do(req)
			if e != nil {
				if route.Virtual {
					s.inflight.targetEnd(route.RouteModelID, targetID)
				}
				attemptCancel()
				class := "upstream_unreachable"
				if errors.Is(e, context.DeadlineExceeded) || isTimeout(e) {
					class = "upstream_timeout"
				}
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: 0, failureClass: class, errorMessage: strPtrIfNonEmpty(fixedUpstreamErrorMessage(class)), latencyMs: time.Since(attemptStart).Milliseconds()})
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				nonTranslationFailure = true
				if r.Context().Err() != nil {
					if errors.Is(r.Context().Err(), context.DeadlineExceeded) {
						class = "client_timeout"
					} else {
						class = "client_cancelled"
					}
					row.httpStatus = 502
					row.errorText = strPtr(class)
					row.fallbackReason = strPtr(class)
					inferenceError(w, 502, "api_error", class, "The client request ended before fallback could complete.", incoming == providers.ProtocolMessages)
					return
				}
				if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && cooldownTrigger(class, 0) && r.Context().Err() == nil {
					failedAt := time.Now()
					s.cooldown.set(candidate.ProviderModelID, failedAt, failedAt.Add(time.Duration(cooldownSeconds)*time.Second), candidate.Provider.Name, candidate.UpstreamModelID, row.clientRequestID, class, fixedUpstreamErrorMessage(class))
				}
				if !route.Virtual {
					row.httpStatus = 502
					row.errorText = strPtr(class)
					row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
					inferenceError(w, 502, "api_error", class, "The upstream provider could not complete the request.", incoming == providers.ProtocolMessages)
					return
				}
				row.fallbackUsed = true
				row.fallbackReason = strPtr(class)
				continue
			}
			timedOut := &atomic.Bool{}
			attemptTimedOut = timedOut
			idle = time.AfterFunc(idleTimeout, func() {
				timedOut.Store(true)
				attemptCancel()
			})
			idleBody := response.Body
			response.Body = bufferedReadCloser{Reader: &idleReader{reader: idleBody, timer: idle}, closer: idleBody}
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				if route.Virtual {
					s.inflight.targetEnd(route.RouteModelID, targetID)
				}
				class := fmt.Sprintf("http_%d", response.StatusCode)
				var upstreamErrorBody []byte
				var upstreamErrorReadErr error
				if logErrorBodies {
					// Body content is retained only by opt-in detailed error logging
					// and is never passed through to the client.
					upstreamErrorBody, upstreamErrorReadErr = io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes+1))
				}
				response.Body.Close()
				idle.Stop()
				attemptCancel()
				if attemptTimedOut.Load() {
					class = "upstream_timeout"
				}
				attempt := requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: response.StatusCode, failureClass: class, errorMessage: strPtrIfNonEmpty(fixedUpstreamErrorMessage(class)), latencyMs: time.Since(attemptStart).Milliseconds()}
				if logErrorBodies && upstreamErrorReadErr == nil && len(upstreamErrorBody) > 0 {
					attempt.errorBody, attempt.errorBodyTruncated = loggedBody(upstreamErrorBody)
				}
				idle.Stop()
				row.attempts = append(row.attempts, attempt)
				s.logAttempt(row, attempt)
				nonTranslationFailure = true
				// Stale-auth recovery: on 401/403 from an OAuth provider, force a
				// token refresh once per request and retry the same target before
				// falling through to normal virtual fallback. ForceOAuthRefresh
				// transitions auth_state on failure, so a dead refresh token surfaces
				// as reconnect_required without further handling here.
				//
				// Cooldown is recorded only AFTER this recovery path: if the refresh
				// succeeds and the same target is retried, the retry must not see the
				// target as cooled and skip it. If the refreshed retry still 401/403s,
				// cooldown opens on that retry's failure. If the refresh fails, the
				// target becomes unavailable and cooldown may open on the persistent
				// failure.
				if !oauthRefreshed[candidate.Provider.ID] && (response.StatusCode == 401 || response.StatusCode == 403) {
					if descriptor, ok := providers.Lookup(candidate.Provider.Type); ok && descriptor.AuthMode == providers.AuthModeOAuth {
						if refreshErr := s.providers.ForceOAuthRefresh(r.Context(), &candidate.Provider); refreshErr == nil {
							oauthRefreshed[candidate.Provider.ID] = true
							// Propagate the fresh credential to every candidate
							// sharing this provider so later targets don't retry
							// with the stale token that just 401'd.
							for j := range candidates {
								if candidates[j].Provider.ID == candidate.Provider.ID {
									candidates[j].Provider = candidate.Provider
								}
							}
							i--
							continue
						}
					}
				}
				// Cooldown applies only to ordered-fallback virtual models. Fixed
				// virtual routes and direct real-model routes never populate the
				// shared cooldown state.
				if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && cooldownTrigger(class, response.StatusCode) {
					failedAt := time.Now()
					s.cooldown.set(candidate.ProviderModelID, failedAt, failedAt.Add(time.Duration(cooldownSeconds)*time.Second), candidate.Provider.Name, candidate.UpstreamModelID, row.clientRequestID, class, fixedUpstreamErrorMessage("upstream_error"))
				}
				// An upstream HTTP response is an upstream failure regardless of
				// status. Ordered virtual routes try their next target by default;
				// router-side failures (for example translation errors) are handled
				// before this point and must not be hidden by fallback.
				if !route.Virtual || !fallbackStatus(response.StatusCode) {
					row.httpStatus = response.StatusCode
					row.errorText = strPtr("upstream_error")
					row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("upstream_error"))
					if logErrorBodies && upstreamErrorReadErr == nil && len(upstreamErrorBody) > 0 {
						row.errorBody, row.errorBodyTruncated = loggedBody(upstreamErrorBody)
					}
					inferenceError(w, response.StatusCode, "api_error", "upstream_error", fmt.Sprintf("Upstream provider returned HTTP %d.", response.StatusCode), incoming == providers.ProtocolMessages)
					return
				}
				row.fallbackUsed = true
				row.fallbackReason = strPtr(class)
				continue
			}
			if e = preflightResponseLimit(response, maxUpstreamNonStreamBytes); e != nil {
				if route.Virtual {
					s.inflight.targetEnd(route.RouteModelID, targetID)
				}
				response.Body.Close()
				idle.Stop()
				attemptCancel()
				class := "upstream_read_error"
				message := "The upstream provider could not complete the request."
				if attemptTimedOut.Load() {
					class = "upstream_timeout"
				}
				if errors.Is(e, errUpstreamResponseTooLarge) {
					class = "upstream_response_too_large"
					message = "The upstream provider response exceeded Tiller's non-streaming response limit."
				}
				terminalPreflightClass = class
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: 0, failureClass: class, latencyMs: time.Since(attemptStart).Milliseconds()})
				if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && cooldownTrigger(class, 0) {
					failedAt := time.Now()
					s.cooldown.set(candidate.ProviderModelID, failedAt, failedAt.Add(time.Duration(cooldownSeconds)*time.Second), candidate.Provider.Name, candidate.UpstreamModelID, row.clientRequestID, class, fixedUpstreamErrorMessage(class))
				}
				nonTranslationFailure = true
				row.attempts[len(row.attempts)-1].errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				if !route.Virtual || r.Context().Err() != nil {
					row.httpStatus = 502
					row.errorText = strPtr(class)
					row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
					inferenceError(w, 502, "api_error", class, message, incoming == providers.ProtocolMessages)
					return
				}
				row.fallbackUsed = true
				row.fallbackReason = strPtr(class)
				continue
			}
			selected, resp, cancel = candidate, response, attemptCancel
			if selected.Virtual {
				activeTargetID = targetID
			}
			row.attempts = append(row.attempts, requestAttempt{providerModelID: selected.ProviderModelID, provider: selected.Provider.Name, model: selected.UpstreamModelID, result: "success", httpStatus: response.StatusCode, latencyMs: time.Since(attemptStart).Milliseconds()})
			allAttemptedFailed = false
			success = true
			goto routeDone
		}
	}
routeDone:
	// Emit a single logical notification for the routing outcome (fallback or
	// all-targets-failed). This is best-effort and never blocks or alters the
	// client response.
	s.maybeNotify(row, route, resp)
	if resp == nil {
		if terminalPreflightClass == "upstream_response_too_large" {
			row.httpStatus = 502
			row.errorText = strPtr(terminalPreflightClass)
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(terminalPreflightClass))
			inferenceError(w, 502, "api_error", terminalPreflightClass, "The upstream provider response exceeded Tiller's non-streaming response limit.", incoming == providers.ProtocolMessages)
			return
		}
		if translationFailureClass != "" && !nonTranslationFailure {
			row.httpStatus = 400
			row.errorText = strPtr(translationFailureClass)
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(translationFailureClass))
			inferenceError(w, 400, "invalid_request_error", translationFailureClass, "The request could not be represented by any configured target.", incoming == providers.ProtocolMessages)
			return
		}
		if protocolUnavailable {
			row.httpStatus = 400
			row.errorText = strPtr("protocol_unavailable")
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("protocol_unavailable"))
			inferenceError(w, 400, "invalid_request_error", "protocol_unavailable", "The selected model does not support this client protocol.", incoming == providers.ProtocolMessages)
			return
		}
		if route.Virtual && allSkippedUnsupportedFeature(row.attempts) {
			row.httpStatus = 400
			row.errorText = strPtr("unsupported_feature")
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("unsupported_feature"))
			inferenceError(w, 400, "invalid_request_error", "unsupported_feature", "The request could not be represented by any configured target.", incoming == providers.ProtocolMessages)
			return
		}
		row.httpStatus = 503
		for i := len(row.attempts) - 1; i >= 0; i-- {
			if row.attempts[i].result == "failed" && row.attempts[i].errorMessage != nil {
				row.errorMessage = row.attempts[i].errorMessage
				break
			}
		}
		if route.Virtual {
			row.errorText = strPtr("virtual_model_unavailable")
			inferenceError(w, 503, "service_unavailable_error", "virtual_model_unavailable", "The virtual model could not be served by its configured targets.", incoming == providers.ProtocolMessages)
		} else {
			row.errorText = strPtr("model_unavailable")
			inferenceError(w, 503, "service_unavailable_error", "model_unavailable", "The configured model is unavailable.", incoming == providers.ProtocolMessages)
		}
		return
	}
	defer cancel()
	clearSelectedCooldown := func() {
		if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && selected.ProviderModelID != "" {
			s.cooldown.remove(selected.ProviderModelID)
		}
	}
	row.resolvedProvider = &selected.Provider.Name
	row.resolvedModel = &selected.UpstreamModelID
	s.inflight.clientResolved(row.clientKeyID, selected.Provider.Name+"/"+selected.UpstreamModelID)
	defer resp.Body.Close()
	copySafeResponseHeaders(w.Header(), resp.Header)
	if v := resp.Header.Get("Request-Id"); v != "" {
		row.providerRequestID = &v
	} else if v := resp.Header.Get("X-Request-Id"); v != "" {
		row.providerRequestID = &v
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	defer idle.Stop()
	reader := resp.Body
	usage := &usageCapture{}
	if translated {
		streamingResponse := isStreamingResponse(resp)
		if streamingResponse {
			streamed = true
			row.streaming = true
			if route.Virtual {
				s.inflight.streaming(route.RouteModelID)
			}
			s.inflight.clientStreaming(row.clientKeyID)
		}
		w.WriteHeader(resp.StatusCode)
		row.httpStatus = resp.StatusCode
		if err := translateResponse(w, reader, incoming, target, selected, usage); err != nil {
			idle.Stop()
			class := "translation_error"
			if attemptTimedOut.Load() {
				class = "upstream_timeout"
			}
			row.httpStatus = 502
			row.errorText = strPtr(class)
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
			markLastAttemptFailed(row, class)
			if streamingResponse {
				writeStreamFailure(w, incoming, class, "tiller_"+row.clientRequestID, selected.RequestedModel)
			}
			s.logger.Warn("protocol translation stream ended", "protocol", incoming, "upstream_protocol", target, "error_class", fmt.Sprintf("%T", err))
		} else {
			clearSelectedCooldown()
		}
		row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
		row.cacheReadInputTokens, row.cacheCreationInputTokens = usage.cacheReadInputTokens, usage.cacheCreationInputTokens
		return
	}
	if isStreamingResponse(resp) {
		streamed = true
		row.streaming = true
		if route.Virtual {
			s.inflight.streaming(route.RouteModelID)
		}
		s.inflight.clientStreaming(row.clientKeyID)
		w.WriteHeader(resp.StatusCode)
		row.httpStatus = resp.StatusCode
		if err := rewriteSSE(w, reader, selected.UpstreamModelID, selected.RequestedModel, usage); err != nil {
			class := "upstream_read_error"
			if attemptTimedOut.Load() {
				class = "upstream_timeout"
			}
			row.httpStatus = 502
			row.errorText = strPtr(class)
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
			markLastAttemptFailed(row, class)
			writeStreamFailure(w, incoming, class, "tiller_"+row.clientRequestID, selected.RequestedModel)
		} else {
			clearSelectedCooldown()
		}
		row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
		row.cacheReadInputTokens, row.cacheCreationInputTokens = usage.cacheReadInputTokens, usage.cacheCreationInputTokens
		return
	}
	// Non-streaming JSON body: read fully to extract usage, then rewrite.
	body, err = io.ReadAll(reader)
	if err != nil {
		class := "upstream_read_error"
		if attemptTimedOut.Load() {
			class = "upstream_timeout"
		}
		row.httpStatus = 502
		row.errorText = strPtr(class)
		row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
		markLastAttemptFailed(row, class)
		return
	}
	extractUsage(body, usage)
	row.inputTokens, row.outputTokens = usage.inputTokens, usage.outputTokens
	row.cacheReadInputTokens, row.cacheCreationInputTokens = usage.cacheReadInputTokens, usage.cacheCreationInputTokens
	w.WriteHeader(resp.StatusCode)
	row.httpStatus = resp.StatusCode
	_, _ = w.Write(rewriteModelBytes(body, selected.UpstreamModelID, selected.RequestedModel))
	clearSelectedCooldown()
}

func markLastAttemptFailed(row *logRow, class string) {
	if len(row.attempts) == 0 {
		return
	}
	attempt := &row.attempts[len(row.attempts)-1]
	attempt.result = "failed"
	attempt.failureClass = class
	attempt.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
}

type bufferedReadCloser struct {
	io.Reader
	closer io.Closer
}

func isStreamingResponse(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

func (r bufferedReadCloser) Close() error { return r.closer.Close() }

// preflightResponseLimit ensures a successful upstream response has produced
// data before Tiller commits anything to the client. This preserves the
// no-splice rule while allowing a different virtual target after a pre-output
// failure.
func preflightResponseLimit(resp *http.Response, limit int64) error {
	if isStreamingResponse(resp) {
		first := make([]byte, 1)
		n, err := resp.Body.Read(first)
		if n == 0 && err != nil {
			return err
		}
		resp.Body = bufferedReadCloser{Reader: io.MultiReader(bytes.NewReader(first[:n]), resp.Body), closer: resp.Body}
		return nil
	}
	// Fail fast on a declared over-limit body without buffering up to 64MiB
	// first. Unknown-length bodies still fall through to the bounded read.
	if resp.ContentLength > limit {
		return errUpstreamResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return errUpstreamResponseTooLarge
	}
	resp.Body = bufferedReadCloser{Reader: bytes.NewReader(body), closer: resp.Body}
	return nil
}

type idleReader struct {
	reader io.Reader
	timer  *time.Timer
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.reader.Read(p)
	if n > 0 {
		// Simple reset. Residual boundary race, documented honestly: if the
		// AfterFunc already fired (or is executing) concurrently with this
		// reset, the cancel still runs and the stream ends early. Stop/drain
		// cannot recall an in-flight callback, so no reset scheme eliminates
		// it — the window is microseconds at exactly idleTimeout of silence
		// and the behaviour predates this code.
		i.timer.Reset(idleTimeout)
	}
	return n, err
}

func copySafeResponseHeaders(dst, src http.Header) {
	for _, name := range []string{"Content-Type", "Cache-Control", "Retry-After", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Request-Id", "X-Request-Id"} {
		if value := src.Values(name); len(value) > 0 {
			dst.Del(name)
			for _, v := range value {
				dst.Add(name, v)
			}
		}
	}
}

// openCodeSessionID resolves the x-opencode-session value for OpenCode
// upstream requests: forward a client-supplied session ID when present so
// native clients keep conversation affinity, else synthesize a stable ID
// from the router request ID (shared across fallback attempts).
//
// A client-supplied value is namespaced by the Tiller client key so two
// different Tiller clients can never accidentally share the same OpenCode
// conversation affinity on a shared real target. The result is a stable,
// opaque hash of (client key, supplied session) prefixed with a truncated
// client key fingerprint, all bounded to 128 bytes.
func openCodeSessionID(clientValue, requestID, clientKeyID string) string {
	if v := strings.TrimSpace(clientValue); v != "" {
		h := sha256.Sum256([]byte(clientKeyID + "\x00" + v))
		return "tiller-" + shortKeyID(clientKeyID) + "-" + hex.EncodeToString(h[:])[:16] + "-" + truncateSession(v)
	}
	if strings.TrimSpace(requestID) == "" {
		return "tiller-anonymous"
	}
	return "tiller-" + strings.TrimSpace(requestID)
}

// shortKeyID returns a stable, truncated fingerprint of a client key ID, kept
// short enough to survive the 128-byte OpenCode session cap after namespacing.
func shortKeyID(id string) string {
	if id == "" {
		return "anon"
	}
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// truncateSession bounds the client-supplied session value so the full
// namespaced result stays within the 128-byte upstream cap.
func truncateSession(v string) string {
	const budget = 40
	if len(v) <= budget {
		return v
	}
	return v[:budget]
}

func copySafeFeatureHeaders(dst, src http.Header, target providers.Protocol) {
	var names []string
	switch target {
	case providers.ProtocolMessages:
		names = []string{"anthropic-beta"}
	case providers.ProtocolChat, providers.ProtocolResponses:
		names = []string{"OpenAI-Beta"}
	}
	for _, name := range names {
		values := headerTokens(dst.Values(name))
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			seen[strings.ToLower(value)] = true
		}
		for _, value := range headerTokens(src.Values(name)) {
			key := strings.ToLower(value)
			if !seen[key] {
				values = append(values, value)
				seen[key] = true
			}
		}
		if len(values) > 0 {
			dst.Set(name, strings.Join(values, ", "))
		}
	}
}

func headerTokens(values []string) []string {
	var tokens []string
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" {
				tokens = append(tokens, token)
			}
		}
	}
	return tokens
}
func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

func fallbackStatus(status int) bool {
	return status < 200 || status >= 300
}

func compatibleProtocol(protocols []providers.Protocol, native providers.Protocol, incoming providers.Protocol) providers.Protocol {
	if native != "" {
		return native
	}
	if providers.Supports(protocols, incoming) {
		return incoming
	}
	for _, candidate := range []providers.Protocol{providers.ProtocolMessages, providers.ProtocolChat, providers.ProtocolResponses} {
		if providers.Supports(protocols, candidate) {
			return candidate
		}
	}
	return ""
}

// checkMinOutputTokens enforces a provider's minimum output length without
// ever overriding an explicit client limit. It returns the (possibly
// default-filled) body and whether the target is compatible:
//   - field absent: the minimum is supplied as the default (overrides
//     nothing) and the target is compatible;
//   - field present and >= min: untouched, compatible;
//   - field present and < min: incompatible — the caller skips the target
//     (virtual) or returns a 400 (direct).
//
// Some upstreams reject values below a threshold (e.g. OpenCode Free
// requires >= 16). A non-numeric field value is left untouched for the
// upstream to validate.
func checkMinOutputTokens(body []byte, minOut int, protocol providers.Protocol) (out []byte, compatible bool, err error) {
	var field string
	switch protocol {
	case providers.ProtocolResponses:
		field = "max_output_tokens"
	case providers.ProtocolChat:
		field = "max_tokens"
	default:
		return body, true, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, false, err
	}
	maxAny, ok := parsed[field]
	if !ok {
		parsed[field] = minOut
		out, err := json.Marshal(parsed)
		if err != nil {
			return nil, false, err
		}
		return out, true, nil
	}
	var maxVal int64
	integral := true
	switch n := maxAny.(type) {
	case int:
		maxVal = int64(n)
	case int64:
		maxVal = n
	case float64:
		if n >= 0 && n == float64(int64(n)) {
			maxVal = int64(n)
		} else {
			integral = false
		}
	default:
		return body, true, nil
	}
	if !integral {
		return nil, false, nil
	}
	if maxVal >= int64(minOut) {
		return body, true, nil
	}
	return body, false, nil
}

// cooldownTrigger reports whether a failure class / HTTP status should open the
// fallback cooldown for the target.
//
// The credential on a target is a single shared upstream key, not a per-client
// pass-through, so an auth/quota rejection (401, 402, 403) is target health
// and cools — hammering a target whose key is dead or out of credit just burns
// time. 400 also cools: relays like the OpenCode free/zen tier surface
// provider-side unavailability ("Model is unavailable", wrapped provider
// errors) as HTTP 400, and router-side client errors (invalid JSON, request too
// large, translation failures, free_model_requires_keyless) are rejected
// before any upstream attempt, so an upstream 400 that survives to this point
// is overwhelmingly a target-side signal.
//
// Only request-specific signals stay excluded: 409, 422, 405, 413 and 415
// commonly reflect the individual request (conflict, validation, wrong
// method, oversized payload, unsupported media) rather than the health or
// availability of the upstream model, and must not hide a working model
// chain-wide for other clients. upstream_response_too_large is a
// router-side guard and never reaches this helper as a status.
func cooldownTrigger(class string, httpStatus int) bool {
	switch class {
	case "upstream_unreachable", "upstream_timeout", "upstream_read_error":
		return true
	}
	if httpStatus == 0 {
		return false
	}
	switch httpStatus {
	case 405, 409, 413, 415, 422:
		return false
	}
	return httpStatus >= 400 && httpStatus < 600
}

// allSkippedUnsupportedFeature reports whether every recorded attempt was a
// skipped min-output-incompatibility. This distinguishes "the request cannot
// be represented by any target" (client's fault, 400) from "every target
// failed or was unavailable" (503) on virtual routes.
func allSkippedUnsupportedFeature(attempts []requestAttempt) bool {
	if len(attempts) == 0 {
		return false
	}
	for _, a := range attempts {
		if a.result != "skipped" || a.failureClass != "unsupported_feature" {
			return false
		}
	}
	return true
}

func rewriteSSE(w http.ResponseWriter, r io.Reader, upstream, requested string, usage *usageCapture) error {
	reader := bufio.NewReader(r)
	flusher, _ := w.(http.Flusher)
	for {
		var line []byte
		var err error
		for {
			var fragment []byte
			fragment, err = reader.ReadSlice('\n')
			line = append(line, fragment...)
			if len(line) > maxSSELineBytes {
				return errors.New("SSE line exceeds limit")
			}
			if err != bufio.ErrBufferFull {
				break
			}
		}
		if len(line) > 0 {
			done := false
			trim := bytes.TrimSpace(line)
			if bytes.HasPrefix(trim, []byte("data:")) {
				payload := bytes.TrimSpace(bytes.TrimPrefix(trim, []byte("data:")))
				if bytes.Equal(payload, []byte("[DONE]")) {
					done = true
				} else {
					var value any
					if json.Unmarshal(payload, &value) == nil {
						if m, ok := value.(map[string]any); ok {
							if u, ok := m["usage"].(map[string]any); ok {
								setUsage(usage, u["prompt_tokens"], u["completion_tokens"])
								setCacheFromUsage(u, usage)
							}
						}
						rewriteModel(value, upstream, requested)
						if encoded, e := json.Marshal(value); e == nil {
							prefix := line[:bytes.Index(line, []byte("data:"))]
							line = append(append(append(prefix, []byte("data: ")...), encoded...), '\n')
						}
					}
				}
			}
			_, _ = w.Write(line)
			if flusher != nil {
				flusher.Flush()
			}
			if done {
				return nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func rewriteModel(value any, upstream, requested string) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if key == "model" {
				if model, ok := item.(string); ok && model == upstream {
					v[key] = requested
				}
			} else {
				rewriteModel(item, upstream, requested)
			}
		}
	case []any:
		for _, item := range v {
			rewriteModel(item, upstream, requested)
		}
	}
}

func (s *Server) logAttempt(row *logRow, attempt requestAttempt) {
	if s.logger == nil {
		return
	}
	var errorMsg string
	if attempt.errorMessage != nil {
		errorMsg = *attempt.errorMessage
	}
	attrs := []any{
		"client_request_id", row.clientRequestID,
		"requested_model", row.requestedModel,
		"provider", attempt.provider,
		"model", attempt.model,
		"http_status", attempt.httpStatus,
		"failure_class", attempt.failureClass,
		"latency_ms", attempt.latencyMs,
		"error", errorMsg,
	}
	if attempt.result == "failed" {
		s.logger.Warn("provider request failed", attrs...)
	} else {
		s.logger.Debug("provider request skipped", attrs...)
	}
}
