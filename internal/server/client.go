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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tiller-router/tiller-router/internal/auth"
	"github.com/tiller-router/tiller-router/internal/database"
	"github.com/tiller-router/tiller-router/internal/providers"
	"github.com/tiller-router/tiller-router/internal/store"
)

const maxUpstreamNonStreamBytes int64 = 64 << 20

// maxUpstreamErrorBytes bounds how much of an upstream error response body we
// read for the sanitized client error. When detailed error logging is
// enabled, the bounded body is also retained on the activity row; otherwise
// only the sanitized summary is kept. The read itself always happens (it
// feeds the client error and the virtual fallback error list) but is bounded
// by upstreamErrorReadTimeout so a stalled body never holds the fallback
// chain hostage.
const maxUpstreamErrorBytes int64 = 1 << 20

var errUpstreamResponseTooLarge = errors.New("upstream response exceeds the non-streaming response limit")

func (s *Server) clientModels(w http.ResponseWriter, r *http.Request) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	sc := s.scope(r)
	keyType, err := sc.ClientKeyType(r.Context(), identity.ID)
	if err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	anthropic := isAnthropicRequest(r)
	if keyType == "single" {
		binding, found, err := sc.GetSingleBinding(r.Context(), identity.ID)
		if err != nil {
			inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
			return
		}
		if !found {
			inferenceError(w, 500, "server_error", "invalid_single_binding", "No binding configured for this API key.", false)
			return
		}
		var contextLength, maxOutputTokens sql.NullInt64
		var caps modelCapabilities
		var reasoningCaps *providers.ReasoningCapabilities
		if binding.RealModelID.Valid {
			pc, err := sc.ProviderModelCapsByID(r.Context(), binding.RealModelID.String)
			if err != nil {
				inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
				return
			}
			contextLength, maxOutputTokens = pc.ContextLength, pc.MaxOutputTokens
			caps = modelCapabilities{Tools: pc.SupportsTools, Vision: pc.SupportsVision, Reasoning: pc.SupportsReasoning, StructuredOutput: pc.SupportsStructuredOutput}
			reasoningCaps = decodeReasoningCapabilities(pc.ReasoningCapabilities)
		} else {
			aggregated, err := s.loadVirtualCapabilities(r.Context(), identity.AccountID, []string{binding.VirtualModelID.String})
			if err != nil {
				inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
				return
			}
			contextLength, maxOutputTokens, caps, reasoningCaps = catalogueCapabilityFields(aggregated[binding.VirtualModelID.String])
		}
		entry := buildCatalogueEntry(binding.ModelName, contextLength, maxOutputTokens, caps, reasoningCaps, anthropic)
		writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{entry}})
		return
	}
	entries, err := sc.ListCatalogueEntries(r.Context(), identity.ID)
	if err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	virtualIDs := make([]string, 0)
	for i := range entries {
		if entries[i].VirtualModelID.Valid {
			virtualIDs = append(virtualIDs, entries[i].VirtualModelID.String)
		}
	}
	aggregated, err := s.loadVirtualCapabilities(r.Context(), identity.AccountID, virtualIDs)
	if err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model catalogue.", false)
		return
	}
	data := []map[string]any{}
	for i := range entries {
		row := &entries[i]
		contextLength, maxOutputTokens := row.ContextLength, row.MaxOutputTokens
		caps := modelCapabilities{Tools: row.SupportsTools, Vision: row.SupportsVision, Reasoning: row.SupportsReasoning, StructuredOutput: row.SupportsStructuredOutput}
		reasoningCaps := decodeReasoningCapabilities(row.ReasoningCapabilities)
		if row.VirtualModelID.Valid {
			contextLength, maxOutputTokens, caps, reasoningCaps = catalogueCapabilityFields(aggregated[row.VirtualModelID.String])
		}
		data = append(data, buildCatalogueEntry(row.Canonical, contextLength, maxOutputTokens, caps, reasoningCaps, anthropic))
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// clientModel handles model-detail lookups. A Single key has one configured
// route, so the requested path is intentionally ignored just like it is for
// inference requests.
func (s *Server) clientModel(w http.ResponseWriter, r *http.Request) {
	identity := r.Context().Value(clientKey).(auth.ClientIdentity)
	sc := s.scope(r)
	keyType, err := sc.ClientKeyType(r.Context(), identity.ID)
	if err != nil {
		inferenceError(w, 500, "server_error", "database_error", "Could not load the model metadata.", false)
		return
	}
	if keyType != "single" {
		inferenceError(w, 404, "invalid_request_error", "model_not_found", "Model not found.", false)
		return
	}
	binding, found, err := sc.GetSingleBinding(r.Context(), identity.ID)
	if err != nil || !found {
		inferenceError(w, 500, "server_error", "invalid_single_binding", "Could not load the Single model binding.", false)
		return
	}
	var contextLength, maxOutputTokens sql.NullInt64
	var caps modelCapabilities
	if binding.RealModelID.Valid {
		pc, err := sc.ProviderModelCapsByID(r.Context(), binding.RealModelID.String)
		if err != nil {
			inferenceError(w, 500, "server_error", "invalid_single_binding", "Could not load the Single model metadata.", false)
			return
		}
		contextLength, maxOutputTokens = pc.ContextLength, pc.MaxOutputTokens
		caps = modelCapabilities{Tools: pc.SupportsTools, Vision: pc.SupportsVision, Reasoning: pc.SupportsReasoning, StructuredOutput: pc.SupportsStructuredOutput}
		entry := buildCatalogueEntry(binding.ModelName, contextLength, maxOutputTokens, caps, decodeReasoningCapabilities(pc.ReasoningCapabilities), isAnthropicRequest(r))
		writeJSON(w, 200, entry)
		return
	}
	aggregated, err := s.loadVirtualCapabilities(r.Context(), identity.AccountID, []string{binding.VirtualModelID.String})
	if err != nil {
		inferenceError(w, 500, "server_error", "invalid_single_binding", "Could not load the Single model metadata.", false)
		return
	}
	contextLength, maxOutputTokens, caps, reasoningCaps := catalogueCapabilityFields(aggregated[binding.VirtualModelID.String])
	entry := buildCatalogueEntry(binding.ModelName, contextLength, maxOutputTokens, caps, reasoningCaps, isAnthropicRequest(r))
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

func (s *Server) loadVirtualCapabilities(ctx context.Context, accountID string, virtualModelIDs []string) (map[string]aggregatedVirtualCapabilities, error) {
	result := make(map[string]aggregatedVirtualCapabilities, len(virtualModelIDs))
	if len(virtualModelIDs) == 0 {
		return result, nil
	}
	rows, err := s.scopeFor(accountID).VirtualTargetCapabilities(ctx, virtualModelIDs)
	if err != nil {
		return nil, err
	}
	targets := make(map[string][]virtualTargetCapabilities, len(virtualModelIDs))
	for i := range rows {
		row := &rows[i]
		targets[row.VirtualModelID] = append(targets[row.VirtualModelID], virtualTargetCapabilities{
			ContextLength: nullInt64Ptr(row.ContextLength), MaxOutputTokens: nullInt64Ptr(row.MaxOutputTokens),
			SupportsTools: triBoolFromInt(row.SupportsTools), SupportsVision: triBoolFromInt(row.SupportsVision),
			SupportsReasoning: triBoolFromInt(row.SupportsReasoning), SupportsStructuredOutput: triBoolFromInt(row.SupportsStructuredOutput),
			ReasoningCapabilities: decodeReasoningCapabilities(row.ReasoningCapabilities),
		})
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
	// CredentialsLocked reports that this target's credential could not be
	// decrypted because the master key is missing or wrong.
	CredentialsLocked bool
	// ReasoningCapabilities holds the normalized selector metadata for this
	// real target. nil when unknown.
	ReasoningCapabilities *providers.ReasoningCapabilities
	MaxOutputTokens       sql.NullInt64
}

func (s *Server) resolveRoute(ctx context.Context, accountID, clientID, requested string) (resolvedRoute, error) {
	sc := s.scopeFor(accountID)
	var route resolvedRoute
	var clientModel string
	err := sc.RunTx(ctx, &sql.TxOptions{ReadOnly: true}, func(tx *store.Scope) error {
		keyType, err := tx.ClientKeyType(ctx, clientID)
		if err != nil {
			return err
		}
		clientModel = requested
		if keyType == "single" {
			binding, found, err := tx.GetSingleBinding(ctx, clientID)
			if err != nil {
				return err
			}
			if !found {
				return sql.ErrNoRows
			}
			clientModel = binding.ModelName
			if binding.RealModelID.Valid {
				route.RouteKind, route.RouteModelID = "real", binding.RealModelID.String
				route.ProviderModelID = binding.RealModelID.String
				info, err := tx.RealModelForID(ctx, binding.RealModelID.String)
				if err != nil {
					return err
				}
				route.RouteModel = info.Canonical
				route.MaxOutputTokens = info.MaxOutputTokens
				route.ReasoningCapabilities = decodeReasoningCapabilities(info.ReasoningCapabilities)
			} else {
				route.RouteKind, route.RouteModelID = "virtual", binding.VirtualModelID.String
				canonical, err := tx.VirtualModelCanonical(ctx, binding.VirtualModelID.String)
				if err != nil {
					return err
				}
				route.RouteModel = canonical
			}
		} else {
			info, err := tx.PermittedRealModel(ctx, clientID, requested)
			if err == nil {
				route.RouteKind, route.RouteModelID, route.RouteModel = "real", info.ModelID, info.Canonical
				route.MaxOutputTokens = info.MaxOutputTokens
				route.ReasoningCapabilities = decodeReasoningCapabilities(info.ReasoningCapabilities)
			} else if errors.Is(err, sql.ErrNoRows) {
				virtualID, canonical, verr := tx.PermittedVirtualModel(ctx, clientID, requested)
				if verr != nil {
					return verr
				}
				route.RouteKind, route.RouteModelID, route.RouteModel = "virtual", virtualID, canonical
			} else {
				return err
			}
		}
		if route.RouteKind == "virtual" {
			mode, err := tx.VirtualRoutingMode(ctx, route.RouteModelID)
			if err != nil {
				return err
			}
			route.RoutingMode = mode
			targets, err := tx.VirtualRouteTargets(ctx, route.RouteModelID)
			if err != nil {
				return err
			}
			locked := 0
			for i := range targets {
				target := routeTargetToResolved(targets[i])
				target.Virtual, target.RequestedModel = true, clientModel
				target.RouteKind, target.RouteModelID, target.RouteModel = route.RouteKind, route.RouteModelID, route.RouteModel
				if target.CredentialsLocked {
					locked++
				}
				route.Targets = append(route.Targets, target)
			}
			route.Virtual, route.RequestedModel = true, clientModel
			if len(route.Targets) > 0 {
				route.Provider, route.UpstreamModelID, route.Available = route.Targets[0].Provider, route.Targets[0].UpstreamModelID, route.Targets[0].Available
			}
			// Every target is locked, so no fallback can serve the request.
			// Surface the explicit credentials-locked condition.
			if len(route.Targets) > 0 && locked == len(route.Targets) {
				return store.ErrSecretsLocked
			}
			return nil
		}
		route.ProviderModelID = route.RouteModelID
		target, err := tx.RealRouteTarget(ctx, route.RouteModelID)
		if err != nil {
			return err
		}
		resolved := routeTargetToResolved(target)
		if resolved.CredentialsLocked {
			return store.ErrSecretsLocked
		}
		route.Provider = resolved.Provider
		route.UpstreamModelID = resolved.UpstreamModelID
		route.NativeProtocol = resolved.NativeProtocol
		route.ReasoningCapabilities = resolved.ReasoningCapabilities
		route.RequestedModel = clientModel
		route.Virtual = false
		route.Available = resolved.Available
		return nil
	})
	if err != nil {
		return resolvedRoute{}, err
	}
	// OAuth hydration happens outside the transaction: Current() can trigger a
	// network token refresh, and holding a SQLite read tx across that
	// serializes all other DB access.
	if route.RouteKind == "virtual" {
		for i := range route.Targets {
			s.providers.HydrateOAuth(ctx, accountID, &route.Targets[i].Provider)
		}
	} else {
		s.providers.HydrateOAuth(ctx, accountID, &route.Provider)
	}
	return route, nil
}

// upstreamHTTPClient returns the HTTP client carrying the account's configured
// time-to-first-header bound. An account with no explicit setting uses the
// registry's default client.
func (s *Server) upstreamHTTPClient(r *http.Request) *http.Client {
	if raw, err := s.scope(r).GetSetting(r.Context(), store.SettingFallbackTimeoutSeconds); err == nil {
		if seconds, perr := strconv.Atoi(raw); perr == nil && seconds > 0 {
			return s.providers.Registry().ClientFor(time.Duration(seconds) * time.Second)
		}
	}
	return s.providers.Registry().HTTPClient()
}

func routeTargetToResolved(t store.RouteTarget) resolvedRoute {
	var target resolvedRoute
	target.ProviderModelID = t.ProviderModelID
	target.Provider = providers.Instance{ID: t.ProviderID, Name: t.ProviderName, Type: t.ProviderType, BaseURL: t.BaseURL, Credential: t.Credential, Enabled: t.ProviderEnabled}
	target.Provider.Protocols = providers.DecodeProtocols(t.Protocols)
	if d, ok := providers.Lookup(t.ProviderType); ok {
		target.Provider.MinOutputTokens = d.MinOutputTokens
	}
	if t.NativeProtocol.Valid {
		target.NativeProtocol = providers.Protocol(t.NativeProtocol.String)
	}
	target.ReasoningCapabilities = decodeReasoningCapabilities(t.ReasoningCapabilities)
	target.Available = t.Available
	target.CredentialsLocked = t.Locked
	target.MaxOutputTokens = t.MaxOutputTokens
	target.UpstreamModelID = t.UpstreamModelID
	return target
}

// idleTimeout is how long a successful upstream response may sit silent
// before the router cancels it. It is the idleReader's reset interval and
// the AfterFunc's initial deadline.
const idleTimeout = 5 * time.Minute

// upstreamErrorReadTimeout bounds how long the router waits for an upstream
// error body before giving up and falling back without the sanitized detail.
// Error bodies are small and prompt; a provider that stalls its error body
// must not delay the fallback chain (which the 5-minute idleTimeout would
// otherwise allow). On timeout the caller keeps the generic http_N class.
const upstreamErrorReadTimeout = 1 * time.Second

// readUpstreamErrorBody reads a bounded copy of an upstream error body,
// giving up after upstreamErrorReadTimeout. The timeout path returns a
// non-nil error so callers skip the sanitized detail and proceed with the
// generic message.
func readUpstreamErrorBody(body io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, maxUpstreamErrorBytes+1))
		ch <- result{data, err}
	}()
	select {
	case res := <-ch:
		return res.data, res.err
	case <-time.After(upstreamErrorReadTimeout):
		return nil, context.DeadlineExceeded
	}
}

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
		accountID:       identity.AccountID,
		clientKeyID:     identity.ID,
		clientName:      identity.Name,
		requestedModel:  requested,
		routeStatus:     "unresolved",
		protocol:        string(incoming),
		clientRequestID: newRequestID(),
		createdAt:       database.Now(),
	}
	logErrorBodies, _ := s.scope(r).GetLogErrorBodies(r.Context())
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
		// Single-ticket liveness: the client ticket (started below) is the
		// only request-presence signal; route presence is derived from it.
		// clientEnd carries the route ID, so an empty RouteModelID here
		// (resolution failed before tracking started) emits nothing.
		if clientTracked {
			s.inflight.clientEnd(row.accountID, row.clientKeyID, route.RouteModelID, streamed)
		}
		if activeTargetID != "" {
			s.inflight.targetEnd(row.accountID, route.RouteModelID, activeTargetID)
		}
		s.writeLog(context.Background(), row)
	}()
	w.Header().Set("X-Tiller-Request-Id", row.clientRequestID)

	route, err = s.resolveRoute(r.Context(), identity.AccountID, identity.ID, requested)
	if err == sql.ErrNoRows {
		row.httpStatus = 404
		row.errorText = strPtr("model_not_found")
		row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("model_not_found"))
		inferenceError(w, 404, "invalid_request_error", "model_not_found", "Model not found.", incoming == providers.ProtocolMessages)
		return
	} else if errors.Is(err, store.ErrSecretsLocked) {
		row.httpStatus = 503
		row.errorText = strPtr("provider_credentials_locked")
		row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("provider_credentials_locked"))
		inferenceError(w, 503, "api_error", "provider_credentials_locked", "Provider credentials are unavailable: the encryption key is missing or does not match. An administrator must restore the master key.", incoming == providers.ProtocolMessages)
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
	s.inflight.clientStart(row.accountID, row.clientKeyID, route.RouteModelID, requested)
	clientTracked = true
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
		cooldownSeconds, _ = s.scope(r).GetFallbackCooldownSeconds(r.Context())
	}
	skippedCooled := false
	allAttemptedFailed := true
	var success bool
	// streamKeepalive is created once a streaming client response is committed.
	// Ordered-fallback probing can sit silent while an upstream produces no
	// deltas; the writer emits SSE comment frames so a reverse proxy does not cut
	// the client-facing connection. It is reused by the relay so only one writer
	// ever touches w, and closed once when proxy returns.
	var streamKeepalive *sseKeepaliveWriter
	defer func() {
		if streamKeepalive != nil {
			streamKeepalive.Close()
		}
	}()
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
				s.inflight.targetSkipped(row.accountID, route.RouteModelID, row.clientKeyID, candidate.ProviderModelID, "unavailable")
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				continue
			}
			if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && !bypass && s.cooldown.cooled(row.accountID, candidate.ProviderModelID, attemptStart) {
				skippedCooled = true
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "cooldown", latencyMs: time.Since(attemptStart).Milliseconds()})
				s.inflight.targetSkipped(row.accountID, route.RouteModelID, row.clientKeyID, candidate.ProviderModelID, "cooldown")
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
				s.inflight.targetSkipped(row.accountID, route.RouteModelID, row.clientKeyID, candidate.ProviderModelID, "free_model_requires_keyless")
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				continue
			}
			target = compatibleProtocol(candidate.Provider.Protocols, candidate.NativeProtocol, incoming)
			if target == "" {
				protocolUnavailable = true
				nonTranslationFailure = true
				row.attempts = append(row.attempts, requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "skipped", failureClass: "protocol_unavailable"})
				s.inflight.targetSkipped(row.accountID, route.RouteModelID, row.clientKeyID, candidate.ProviderModelID, "protocol_unavailable")
				s.logAttempt(row, row.attempts[len(row.attempts)-1])
				continue
			}
			translated = target != incoming
			attemptBody := append([]byte(nil), originalBody...)
			codexSessionSource := ""
			codexEffort := ""
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
					s.inflight.targetSkipped(row.accountID, route.RouteModelID, row.clientKeyID, candidate.ProviderModelID, code)
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
						s.inflight.targetSkipped(row.accountID, route.RouteModelID, row.clientKeyID, candidate.ProviderModelID, "unsupported_feature")
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
				attemptBody, err = normalizeCodexRequest(attemptBody, candidate.ReasoningCapabilities)
				if err != nil {
					row.httpStatus = 400
					row.errorText = strPtr("invalid_request")
					row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("invalid_request"))
					inferenceError(w, 400, "invalid_request_error", "invalid_request", "The Codex request could not be normalized.", incoming == providers.ProtocolMessages)
					return
				}
				codexEffort = codexRequestEffort(attemptBody)
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
					s.inflight.targetSkipped(row.accountID, route.RouteModelID, row.clientKeyID, candidate.ProviderModelID, "unsupported_feature")
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
			freeShadow := candidate.Provider.Type == "opencode-free"
			if freeShadow {
				// Anonymous free-tier requests mirror the genuine OpenCode
				// client's wire shape (captured against opencode/1.18.26):
				// "Bearer public" auth, first-party UA, Accept */*, affinity
				// session headers, no relay tells. Zen/go keep the
				// router-identifying headers — only the keyless path needs the
				// shadow, and only there is it approved.
				req.Header.Set("Accept", "*/*")
				req.Header.Set("User-Agent", openCodeFreeUserAgent)
				if clientIP := s.requestClientIP(r); clientIP != "" {
					req.Header.Set("X-Real-IP", clientIP)
				}
			}
			if candidate.Provider.Type == "opencode-zen" || candidate.Provider.Type == "opencode-go" || candidate.Provider.Type == "opencode-free" {
				// OpenCode requires a stable per-conversation identity for
				// routing/prompt-caching. Zen/go send the router's synthesized
				// X-Opencode-Session; the anonymous free shadow mirrors the
				// genuine client, which sends only the affinity
				// headers (captured against opencode/1.18.26) — no
				// X-Opencode-Session at all.
				session := openCodeSessionID(r.Header.Get("x-opencode-session"), row.clientRequestID, row.clientKeyID)
				if freeShadow {
					affinity := freeShadowSession(session)
					req.Header.Set("x-session-affinity", affinity)
					req.Header.Set("x-session-id", affinity)
				} else {
					req.Header.Set("X-Opencode-Session", session)
					req.Header.Set("X-Opencode-Client", "tiller-router")
				}
			}
			if freeShadow {
				// Anonymous free tier authenticates as the public key, exactly
				// as the genuine client does — never with a stored credential.
				req.Header.Set("Authorization", "Bearer public")
			} else {
				providers.ApplyRequestAuth(req, candidate.Provider)
			}
			copySafeFeatureHeaders(req.Header, r.Header, target)
			if candidate.Provider.Type == "codex-subscription" {
				sessionID, source := codexSessionID(clientSessionHeader(r.Header), row.clientRequestID, row.clientKeyID)
				codexSessionSource = source
				req.Header.Set("session-id", sessionID)
				req.Header.Set("x-client-request-id", row.clientRequestID)
				req.Header.Set("x-codex-routing-hint", "model="+candidate.UpstreamModelID)
				// The ChatGPT Codex backend switches to SSE only when Accept is
				// exactly text/event-stream (the official codex_cli_rs sends this
				// exact value). A combined Accept leaves it on the buffered JSON
				// path, which stalls long reasoning generations until a reverse
				// proxy read timeout kills the connection. Set it after
				// ApplyRequestAuth so the Codex-specific contract wins.
				req.Header.Set("Accept", "text/event-stream")
			}
			targetID := candidate.ProviderModelID
			if targetID == "" {
				targetID = candidate.Provider.Name + "/" + candidate.UpstreamModelID
			}
			// Target-level tracking covers direct real-model routes too: for a
			// real route this is the single 1:1 leg, so the Activity graph can
			// light it (and dim it on failure) while the request is in flight.
			s.inflight.targetStart(row.accountID, route.RouteModelID, targetID)
			response, e := s.upstreamHTTPClient(r).Do(req)
			if e != nil {
				s.inflight.targetEnd(row.accountID, route.RouteModelID, targetID)
				attemptCancel()
				class := "upstream_unreachable"
				if errors.Is(e, context.DeadlineExceeded) || isTimeout(e) {
					class = "upstream_timeout"
				}
				// A network error that coincides with the client ending the
				// request is the client's, not the target's: classify it so it
				// never degrades target health or paints a red roundel.
				class = clientFailureClass(r.Context(), class)
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
					s.openCooldown(candidate, class, row, cooldownSeconds, fixedUpstreamErrorMessage(class))
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
			headerLatencyMs := time.Since(attemptStart).Milliseconds()
			streaming := isStreamingResponse(response)
			logCodexResponse := func(firstByteLatencyMs int64) {
				if candidate.Provider.Type != "codex-subscription" || s.logger == nil {
					return
				}
				s.logger.Info("codex upstream response",
					"client_request_id", row.clientRequestID,
					"provider", candidate.Provider.Name,
					"model", candidate.UpstreamModelID,
					"http_status", response.StatusCode,
					"content_type", response.Header.Get("Content-Type"),
					"upstream_streaming", streaming,
					"header_latency_ms", headerLatencyMs,
					"first_byte_latency_ms", firstByteLatencyMs,
					"effort", codexEffort,
					"session_source", codexSessionSource,
				)
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
				s.inflight.targetEnd(row.accountID, route.RouteModelID, targetID)
				class := fmt.Sprintf("http_%d", response.StatusCode)
				// Always read a bounded copy: it is persisted only under opt-in
				// detailed error logging, but a sanitized summary is always used
				// to build the client error (direct routes) or the virtual
				// fallback error list. The read gives up after
				// upstreamErrorReadTimeout so a stalled body falls back
				// without the detail instead of holding the chain hostage.
				upstreamErrorBody, upstreamErrorReadErr := readUpstreamErrorBody(response.Body)
				response.Body.Close()
				idle.Stop()
				attemptCancel()
				if attemptTimedOut.Load() {
					class = "upstream_timeout"
				}
				attempt := requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: response.StatusCode, failureClass: class, errorMessage: strPtrIfNonEmpty(fixedUpstreamErrorMessage(class)), latencyMs: time.Since(attemptStart).Milliseconds()}
				freeTierRejection := upstreamErrorReadErr == nil && isOpenCodeFreeTierRejection(upstreamErrorBody)
				if freeTierRejection {
					// OpenCode's free-tier policy gate fired: reclassify to the
					// router-owned failure class so the client gets a
					// diagnosable remediation instead of relayed Console text.
					// The sanitized upstream detail is still attached below.
					class = "free_tier_rejected"
					attempt.failureClass = class
					attempt.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
				}
				if upstreamErrorReadErr == nil && len(upstreamErrorBody) > 0 {
					if logErrorBodies {
						attempt.errorBody, attempt.errorBodyTruncated = loggedBody(upstreamErrorBody)
					}
					detail := parseUpstreamErrorDetail(upstreamErrorBody, response.Header.Get("Content-Type"))
					attempt.clientError = redactProviderSecrets(detail.clientMessage(), candidate.Provider)
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
						if refreshErr := s.providers.ForceOAuthRefresh(r.Context(), identity.AccountID, &candidate.Provider); refreshErr == nil {
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
					cooldownMessage := fixedUpstreamErrorMessage("upstream_error")
					if class == "free_tier_rejected" {
						cooldownMessage = fixedUpstreamErrorMessage(class)
					}
					s.openCooldown(candidate, class, row, cooldownSeconds, cooldownMessage)
				}
				// An upstream HTTP response is an upstream failure regardless of
				// status. Ordered virtual routes try their next target by default;
				// router-side failures (for example translation errors) are handled
				// before this point and must not be hidden by fallback.
				if !route.Virtual || !fallbackStatus(response.StatusCode) {
					row.httpStatus = response.StatusCode
					errorCode := "upstream_error"
					message := fmt.Sprintf("Upstream provider returned HTTP %d.", response.StatusCode)
					if freeTierRejection {
						// Fail loud with the router-owned remediation on direct
						// routes: the raw Console text names no fix.
						row.httpStatus = 400
						row.errorText = strPtr("free_tier_rejected")
						row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("free_tier_rejected"))
						errorCode = "free_tier_rejected"
						message = "OpenCode declined the free-tier request: it can only be served from within OpenCode. Use a keyed opencode-zen or opencode-go provider for this model."
						if attempt.clientError != "" {
							message = fmt.Sprintf("%s Upstream detail: %s", message, attempt.clientError)
						}
					} else {
						row.errorText = strPtr("upstream_error")
						row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage("upstream_error"))
						if attempt.clientError != "" {
							message = fmt.Sprintf("%s (HTTP %d)", attempt.clientError, response.StatusCode)
						}
					}
					if logErrorBodies && upstreamErrorReadErr == nil && len(upstreamErrorBody) > 0 {
						row.errorBody, row.errorBodyTruncated = loggedBody(upstreamErrorBody)
					}
					inferenceError(w, row.httpStatus, "api_error", errorCode, message, incoming == providers.ProtocolMessages)
					return
				}
				row.fallbackUsed = true
				if freeTierRejection {
					row.fallbackReason = strPtr("free_tier_rejected")
				} else {
					row.fallbackReason = strPtr(class)
				}
				continue
			}
			// Ordered-fallback streaming targets are probed for usable output before
			// committing. That probe (and the preflight read below) can sit silent for
			// a long reasoning prefill, so commit the client stream and start SSE
			// keepalives up front. Comment frames are transport keepalives, not model
			// output, so the chain can still fall through to another target.
			// Only router-owned transport headers are committed before target selection;
			// provider-specific request IDs / rate-limit headers are omitted because
			// the serving provider isn't known yet.
			if route.Virtual && route.RoutingMode == "ordered_fallback" && streaming && streamKeepalive == nil {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(response.StatusCode)
				streamKeepalive = newSSEKeepaliveWriter(w, s.keepaliveInterval())
			}
			preflightStart := time.Now()
			if e = preflightResponseLimit(response, maxUpstreamNonStreamBytes); e != nil {
				if streaming {
					logCodexResponse(time.Since(preflightStart).Milliseconds())
				} else {
					logCodexResponse(0)
				}
				s.inflight.targetEnd(row.accountID, route.RouteModelID, targetID)
				response.Body.Close()
				idle.Stop()
				attemptCancel()
				class := "upstream_read_error"
				message := "The upstream provider could not complete the request."
				if attemptTimedOut.Load() {
					class = "upstream_timeout"
				}
				class = clientFailureClass(r.Context(), class)
				if errors.Is(e, errUpstreamResponseTooLarge) {
					class = "upstream_response_too_large"
					message = "The upstream provider response exceeded Tiller's non-streaming response limit."
				}
				terminalPreflightClass = class
				attempt := requestAttempt{providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name, model: candidate.UpstreamModelID, result: "failed", httpStatus: 0, failureClass: class, latencyMs: time.Since(attemptStart).Milliseconds(), readCause: truncateReadCause(e), clientCtxErr: ctxErrString(r.Context().Err()), attemptTimedOut: attemptTimedOut.Load(), upstreamStreaming: streaming, headerLatencyMs: headerLatencyMs}
				row.attempts = append(row.attempts, attempt)
				// A body-read error caused by the client ending the request is
				// self-inflicted, not evidence the target is unhealthy: never
				// cool it. Mirrors the network-error path above.
				if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && cooldownTrigger(class, 0) && r.Context().Err() == nil {
					s.openCooldown(candidate, class, row, cooldownSeconds, fixedUpstreamErrorMessage(class))
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
			if streaming {
				logCodexResponse(time.Since(preflightStart).Milliseconds())
			} else {
				logCodexResponse(0)
			}
			// Ordered-fallback targets must produce usable output to count as
			// success. A pre-output probe catches relays that return a 2xx with
			// an explicit stream error or an empty/role-only completion, so the
			// chain can advance instead of failing the client with no content.
			if route.Virtual && route.RoutingMode == "ordered_fallback" {
				outcome, probeErr := probeUpstreamOutput(response, target)
				class := ""
				if probeErr != nil {
					class = "upstream_read_error"
					if attemptTimedOut.Load() {
						class = "upstream_timeout"
					}
					class = clientFailureClass(r.Context(), class)
				} else {
					switch outcome {
					case probeStreamError:
						class = "upstream_stream_error"
					case probeEmpty:
						class = "empty_response"
					}
				}
				if class != "" {
					s.inflight.targetEnd(row.accountID, route.RouteModelID, targetID)
					response.Body.Close()
					idle.Stop()
					attemptCancel()
					attempt := requestAttempt{
						providerModelID: candidate.ProviderModelID, provider: candidate.Provider.Name,
						model: candidate.UpstreamModelID, result: "failed", httpStatus: response.StatusCode,
						failureClass: class, latencyMs: time.Since(attemptStart).Milliseconds(),
						errorMessage:    strPtrIfNonEmpty(fixedUpstreamErrorMessage(class)),
						readCause:       truncateReadCause(probeErr),
						clientCtxErr:    ctxErrString(r.Context().Err()),
						attemptTimedOut: attemptTimedOut.Load(), upstreamStreaming: streaming,
						headerLatencyMs: headerLatencyMs,
					}
					nonTranslationFailure = true
					if !s.recordPreOutputFailure(w, r, row, candidate, attempt, class, cooldownSeconds, incoming) {
						return
					}
					continue
				}
			}
			selected, resp, cancel = candidate, response, attemptCancel
			// Track the hot leg for the deferred targetEnd, for virtual and
			// direct real-model routes alike (the 1:1 real leg included).
			activeTargetID = targetID
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
			if streamKeepalive != nil {
				// The client stream was already committed while probing; surface
				// the exhausted chain as an SSE failure frame instead of a JSON
				// body on a 200 stream.
				writeStreamFailure(streamKeepalive, incoming, "virtual_model_unavailable", "tiller_"+row.clientRequestID, "")
				streamKeepalive.Flush()
				return
			}
			inferenceError(w, 503, "service_unavailable_error", "virtual_model_unavailable", exhaustedRouteMessage(row.attempts), incoming == providers.ProtocolMessages)
		} else {
			row.errorText = strPtr("model_unavailable")
			inferenceError(w, 503, "service_unavailable_error", "model_unavailable", "The configured model is unavailable.", incoming == providers.ProtocolMessages)
		}
		return
	}
	defer cancel()
	clearSelectedCooldown := func() {
		if route.RoutingMode == "ordered_fallback" && cooldownSeconds > 0 && selected.ProviderModelID != "" {
			s.cooldown.remove(row.accountID, selected.ProviderModelID)
		}
	}
	row.resolvedProvider = &selected.Provider.Name
	row.resolvedModel = &selected.UpstreamModelID
	s.inflight.clientResolved(row.accountID, row.clientKeyID, route.RouteModelID, selected.Provider.Name+"/"+selected.UpstreamModelID)
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
	// ensureStreamKeepalive commits the streaming status and starts the
	// keepalive writer if the ordered-fallback probe did not already commit
	// it. Reusing the same writer keeps a single goroutine writing to w.
	ensureStreamKeepalive := func() *sseKeepaliveWriter {
		if streamKeepalive == nil {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			streamKeepalive = newSSEKeepaliveWriter(w, s.keepaliveInterval())
		}
		return streamKeepalive
	}
	// streamOut routes terminal stream failures through the keepalive writer
	// when one is active, so no two goroutines write w concurrently.
	streamOut := func() io.Writer {
		if streamKeepalive != nil {
			return streamKeepalive
		}
		return w
	}
	if isStreamingResponse(resp) {
		// Prevent common reverse proxies from buffering the live response until
		// the model has finished generating it.
		w.Header().Set("X-Accel-Buffering", "no")
	}
	if translated {
		streamingResponse := isStreamingResponse(resp)
		if streamingResponse {
			streamed = true
			row.streaming = true
			s.inflight.clientStreaming(row.accountID, row.clientKeyID, route.RouteModelID)
			ensureStreamKeepalive()
		}
		if streamKeepalive == nil {
			w.WriteHeader(resp.StatusCode)
		}
		row.httpStatus = resp.StatusCode
		if err := translateResponse(w, streamKeepalive, reader, incoming, target, selected, usage); err != nil {
			idle.Stop()
			class := "translation_error"
			if attemptTimedOut.Load() {
				class = "upstream_timeout"
			}
			class = clientFailureClass(r.Context(), class)
			row.httpStatus = 502
			row.errorText = strPtr(class)
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
			markLastAttemptFailed(row, class)
			if streamingResponse {
				writeStreamFailure(streamOut(), incoming, class, "tiller_"+row.clientRequestID, selected.RequestedModel)
				if streamKeepalive != nil {
					streamKeepalive.Flush()
				}
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
		s.inflight.clientStreaming(row.accountID, row.clientKeyID, route.RouteModelID)
		ensureStreamKeepalive()
		row.httpStatus = resp.StatusCode
		if err := rewriteSSE(w, streamKeepalive, reader, selected.UpstreamModelID, selected.RequestedModel, usage); err != nil {
			class := "upstream_read_error"
			if attemptTimedOut.Load() {
				class = "upstream_timeout"
			}
			class = clientFailureClass(r.Context(), class)
			row.httpStatus = 502
			row.errorText = strPtr(class)
			row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(class))
			markLastAttemptFailed(row, class)
			writeStreamFailure(streamOut(), incoming, class, "tiller_"+row.clientRequestID, selected.RequestedModel)
			if streamKeepalive != nil {
				streamKeepalive.Flush()
			}
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
		class = clientFailureClass(r.Context(), class)
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

// recordPreOutputFailure records a failed pre-output attempt, opens a target
// cooldown when the failure class is eligible, and marks the request as having
// used fallback. It returns false only when the client context has ended and a
// terminal error was written, in which case the caller must stop the loop.
func (s *Server) recordPreOutputFailure(
	w http.ResponseWriter, r *http.Request, row *logRow, candidate resolvedRoute,
	attempt requestAttempt, class string, cooldownSeconds int, incoming providers.Protocol,
) bool {
	row.attempts = append(row.attempts, attempt)
	s.logAttempt(row, attempt)
	if cooldownSeconds > 0 && cooldownTrigger(class, attempt.httpStatus) && r.Context().Err() == nil {
		s.openCooldown(candidate, class, row, cooldownSeconds, fixedUpstreamErrorMessage(class))
	}
	if r.Context().Err() != nil {
		clientClass := clientFailureClass(r.Context(), class)
		row.httpStatus = 502
		row.errorText = strPtr(clientClass)
		row.errorMessage = strPtrIfNonEmpty(fixedUpstreamErrorMessage(clientClass))
		row.fallbackReason = strPtr(clientClass)
		inferenceError(w, 502, "api_error", clientClass, "The upstream provider could not complete the request.", incoming == providers.ProtocolMessages)
		return false
	}
	row.fallbackUsed = true
	row.fallbackReason = strPtr(class)
	return true
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

// openCodeFreeUserAgent mirrors the genuine OpenCode CLI's User-Agent on
// anonymous free-tier requests (captured against opencode/1.18.26 via local
// echo: "opencode/1.18.26 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14").
// Keyed zen/go traffic keeps Tiller-Router/1 — the shadow applies to the
// approved opencode-free path only.
const openCodeFreeUserAgent = "opencode/1.18.26 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

// freeShadowSession derives the opaque affinity value sent on the shadow
// path from the router-stable session: same stability guarantees as
// openCodeSessionID (stable across fallback attempts, isolated across
// clients) with no "tiller-" tell.
func freeShadowSession(session string) string {
	if v := strings.TrimSpace(session); v != "" {
		h := sha256.Sum256([]byte("opencode-free-shadow\x00" + v))
		return "ses_" + hex.EncodeToString(h[:])[:23]
	}
	return "ses_AAAAAAAAAAAAAAAAAAAAAAA"
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

// clientSessionHeader returns the client-supplied conversation identity from
// the first session header present, or "" when none is provided.
//
// OpenCode only sends x-opencode-session when the provider ID starts with
// "opencode". A third-party OpenAI-compatible provider (Tiller included) gets
// x-session-affinity / X-Session-Id instead, so prompt-cache affinity needs to
// accept all three header shapes.
func clientSessionHeader(h http.Header) string {
	for _, name := range []string{"x-opencode-session", "x-session-affinity", "x-session-id"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

// codexSessionID keeps Codex prompt/cache affinity stable when a client
// supplies a conversation identity, while preserving request isolation for
// generic clients that do not provide one.
func codexSessionID(clientValue, requestID, clientKeyID string) (string, string) {
	if strings.TrimSpace(clientValue) != "" {
		return openCodeSessionID(clientValue, requestID, clientKeyID), "client"
	}
	return openCodeSessionID("", requestID, clientKeyID), "request"
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

func truncateReadCause(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", " ")
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func ctxErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// clientFailureClass overrides a failure class with a client-caused one when
// the request context has ended. A client cancel/timeout is not evidence the
// target is unhealthy, so it must not degrade target health or paint a red
// leg. The router's own per-attempt idle timer cancels the attempt context,
// not the request context, so a genuine upstream timeout still classifies as
// upstream_timeout.
func clientFailureClass(ctx context.Context, fallback string) string {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "client_timeout"
		}
		return "client_cancelled"
	}
	return fallback
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
	case "upstream_unreachable", "upstream_timeout", "upstream_read_error",
		"empty_response", "upstream_stream_error":
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

func rewriteSSE(w http.ResponseWriter, keepalive *sseKeepaliveWriter, r io.Reader, upstream, requested string, usage *usageCapture) error {
	reader := bufio.NewReader(r)
	if keepalive == nil {
		keepalive = newSSEKeepaliveWriter(w, sseKeepaliveInterval)
		defer keepalive.Close()
	}
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
			_, _ = keepalive.Write(line)
			keepalive.Flush()
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

// openCooldown records a fallback cooldown for the target and logs the
// transition so operators can see a target leave rotation and why.
func (s *Server) openCooldown(candidate resolvedRoute, class string, row *logRow, cooldownSeconds int, message string) {
	failedAt := time.Now()
	until := failedAt.Add(time.Duration(cooldownSeconds) * time.Second)
	s.cooldown.set(rowAccountID(row), candidate.ProviderModelID, failedAt, until, candidate.Provider.Name, candidate.UpstreamModelID, row.clientRequestID, class, message)
	if s.logger != nil {
		s.logger.Warn("target cooled down",
			"provider", candidate.Provider.Name,
			"model", candidate.UpstreamModelID,
			"failure_class", class,
			"until", until.Format(time.RFC3339Nano),
			"origin_request_id", row.clientRequestID,
		)
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
		if attempt.readCause != "" {
			attrs = append(attrs, "upstream_read_cause", attempt.readCause)
		}
		attrs = append(attrs, "attempt_timed_out", attempt.attemptTimedOut)
		if attempt.clientCtxErr != "" {
			attrs = append(attrs, "client_ctx_err", attempt.clientCtxErr)
		}
		if attempt.headerLatencyMs > 0 {
			attrs = append(attrs, "header_latency_ms", attempt.headerLatencyMs, "upstream_streaming", attempt.upstreamStreaming)
		}
		s.logger.Warn("provider request failed", attrs...)
		return
	}
	if attempt.failureClass == "cooldown" {
		// A cooldown skip is not an error, but it is operationally important:
		// log it at Info (visible at the default level) with the origin of the
		// failure that opened the cooldown so the skip explains itself.
		if s.cooldown != nil {
			if entry, ok := s.cooldown.statusByName(rowAccountID(row), attempt.provider, attempt.model, time.Now()); ok {
				attrs = append(attrs,
					"cooldown_origin_request_id", entry.originRequestLogID,
					"cooldown_origin_error_class", entry.originErrorClass,
					"cooldown_until", entry.until.Format(time.RFC3339Nano),
				)
			}
		}
		s.logger.Info("provider request skipped", attrs...)
		return
	}
	s.logger.Debug("provider request skipped", attrs...)
}
