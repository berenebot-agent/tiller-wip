package server

import (
	"github.com/tiller-router/tiller-router/internal/providers"
)

// virtualTargetEligible is the one admin-side definition of a target that can
// currently receive traffic. virtualTargetView.Available intentionally omits
// the target's own enabled flag so that the admin UI can still explain why a
// disabled target is unavailable.
func virtualTargetEligible(target virtualTargetView) bool {
	return target.Enabled && target.Available
}

func eligibleVirtualTargets(targets []virtualTargetView) []virtualTargetView {
	eligible := make([]virtualTargetView, 0, len(targets))
	for _, target := range targets {
		if virtualTargetEligible(target) {
			eligible = append(eligible, target)
		}
	}
	return eligible
}

type virtualTargetCapabilities struct {
	ContextLength            *int64
	MaxOutputTokens          *int64
	SupportsTools            *bool
	SupportsVision           *bool
	SupportsReasoning        *bool
	SupportsStructuredOutput *bool
	ReasoningCapabilities    *providers.ReasoningCapabilities
}

type aggregatedVirtualCapabilities struct {
	ContextLength            *int64
	MaxOutputTokens          *int64
	SupportsTools            *bool
	SupportsVision           *bool
	SupportsReasoning        *bool
	SupportsStructuredOutput *bool
	ReasoningCapabilities    *providers.ReasoningCapabilities
}

func aggregateVirtualCapabilities(targets []virtualTargetCapabilities) aggregatedVirtualCapabilities {
	result := aggregatedVirtualCapabilities{}
	if len(targets) == 0 {
		return result
	}
	var contextLength, maxOutputTokens int64
	contextKnown, maxOutputKnown := true, true
	for _, target := range targets {
		if target.ContextLength == nil || *target.ContextLength <= 0 {
			contextKnown = false
		} else if contextKnown && (result.ContextLength == nil || *target.ContextLength < contextLength) {
			contextLength = *target.ContextLength
			result.ContextLength = &contextLength
		}
		if target.MaxOutputTokens == nil || *target.MaxOutputTokens <= 0 {
			maxOutputKnown = false
		} else if maxOutputKnown && (result.MaxOutputTokens == nil || *target.MaxOutputTokens < maxOutputTokens) {
			maxOutputTokens = *target.MaxOutputTokens
			result.MaxOutputTokens = &maxOutputTokens
		}
	}
	if !contextKnown {
		result.ContextLength = nil
	}
	if !maxOutputKnown {
		result.MaxOutputTokens = nil
	}
	result.SupportsTools = aggregateVirtualBool(targets, func(target virtualTargetCapabilities) *bool { return target.SupportsTools })
	result.SupportsVision = aggregateVirtualBool(targets, func(target virtualTargetCapabilities) *bool { return target.SupportsVision })
	result.SupportsReasoning = aggregateVirtualBool(targets, func(target virtualTargetCapabilities) *bool { return target.SupportsReasoning })
	result.SupportsStructuredOutput = aggregateVirtualBool(targets, func(target virtualTargetCapabilities) *bool { return target.SupportsStructuredOutput })
	for _, target := range targets {
		result.ReasoningCapabilities = mergeReasoningCapabilities(result.ReasoningCapabilities, target.ReasoningCapabilities)
	}
	return result
}

func aggregateVirtualBool(targets []virtualTargetCapabilities, value func(virtualTargetCapabilities) *bool) *bool {
	unknown := false
	for _, target := range targets {
		flag := value(target)
		if flag == nil {
			unknown = true
		} else if !*flag {
			result := false
			return &result
		}
	}
	if unknown {
		return nil
	}
	result := true
	return &result
}

func virtualTargetCapability(target virtualTargetView) virtualTargetCapabilities {
	return virtualTargetCapabilities{
		ContextLength: target.ContextLength, MaxOutputTokens: target.MaxOutputTokens,
		SupportsTools: target.SupportsTools, SupportsVision: target.SupportsVision,
		SupportsReasoning: target.SupportsReasoning, SupportsStructuredOutput: target.SupportsStructuredOutput,
		ReasoningCapabilities: target.ReasoningCapabilities,
	}
}

func aggregateVirtualNumeric(targets []virtualTargetView, value func(virtualTargetView) *int64) *int64 {
	eligible := eligibleVirtualTargets(targets)
	capabilities := make([]virtualTargetCapabilities, 0, len(eligible))
	for _, target := range eligible {
		capability := virtualTargetCapability(target)
		if value != nil {
			capability.ContextLength = value(target)
		}
		capabilities = append(capabilities, capability)
	}
	return aggregateVirtualCapabilities(capabilities).ContextLength
}

func aggregateVirtualReasoning(targets []virtualTargetView, caps func(virtualTargetView) *providers.ReasoningCapabilities) *providers.ReasoningCapabilities {
	eligible := eligibleVirtualTargets(targets)
	capabilities := make([]virtualTargetCapabilities, 0, len(eligible))
	for _, target := range eligible {
		capability := virtualTargetCapability(target)
		if caps != nil {
			capability.ReasoningCapabilities = caps(target)
		}
		capabilities = append(capabilities, capability)
	}
	return aggregateVirtualCapabilities(capabilities).ReasoningCapabilities
}

// mergeReasoningCapabilities combines two capability sets into a superset. nil
// entries are treated as unknown and do not contribute. The result is nil only
// when both inputs are nil.
func mergeReasoningCapabilities(a, b *providers.ReasoningCapabilities) *providers.ReasoningCapabilities {
	if a == nil && b == nil {
		return nil
	}
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	// Build a superset: union of effort values, toggle if either reports it,
	// budget with widest known range, default effort from either.
	result := &providers.ReasoningCapabilities{
		DefaultEffort:  a.DefaultEffort,
		Mandatory:      mergeBoolPtr(a.Mandatory, b.Mandatory),
		DefaultEnabled: mergeBoolPtr(a.DefaultEnabled, b.DefaultEnabled),
	}
	if result.DefaultEffort == "" {
		result.DefaultEffort = b.DefaultEffort
	}
	// Union effort values from all effort options. An effort option with no
	// values is OpenRouter's explicit unrestricted form and dominates any
	// finite allowlist in the virtual superset.
	seen := make(map[string]bool)
	hasEffort, unrestrictedEffort := false, false
	for _, opt := range append(a.Options, b.Options...) {
		if opt.Type == providers.ReasoningOptionEffort {
			hasEffort = true
			if len(opt.Values) == 0 {
				unrestrictedEffort = true
			}
			for _, v := range opt.Values {
				seen[v] = true
			}
		}
	}
	if hasEffort {
		var values []string
		if !unrestrictedEffort {
			for v := range seen {
				values = append(values, v)
			}
			values = providers.SortEfforts(values)
		}
		result.Options = append(result.Options, providers.ReasoningOption{
			Type:   providers.ReasoningOptionEffort,
			Values: values,
		})
	}
	// Toggle if either reports it.
	if hasOption(a, providers.ReasoningOptionToggle) || hasOption(b, providers.ReasoningOptionToggle) {
		result.Options = append(result.Options, providers.ReasoningOption{Type: providers.ReasoningOptionToggle})
	}
	// Budget: widest known range.
	if aBudget := findBudget(a); aBudget != nil || findBudget(b) != nil {
		var min, max *int64
		if aBudget != nil {
			min, max = aBudget.Min, aBudget.Max
		}
		if bBudget := findBudget(b); bBudget != nil {
			if bBudget.Min != nil && (min == nil || *bBudget.Min < *min) {
				v := *bBudget.Min
				min = &v
			}
			if bBudget.Max != nil && (max == nil || *bBudget.Max > *max) {
				v := *bBudget.Max
				max = &v
			}
		}
		result.Options = append(result.Options, providers.ReasoningOption{
			Type: providers.ReasoningOptionBudgetTokens,
			Min:  min,
			Max:  max,
		})
	}
	// Merge parameters.
	paramSeen := make(map[string]bool)
	for _, p := range append(a.Parameters, b.Parameters...) {
		if !paramSeen[p] {
			paramSeen[p] = true
			result.Parameters = append(result.Parameters, p)
		}
	}
	// Keep the Anthropic distinction between adaptive and legacy enabled
	// thinking. The order is stable for deterministic catalogue JSON.
	seenModes := make(map[string]bool)
	for _, mode := range append(a.ThinkingModes, b.ThinkingModes...) {
		if !seenModes[mode] {
			seenModes[mode] = true
			result.ThinkingModes = append(result.ThinkingModes, mode)
		}
	}
	if len(result.ThinkingModes) > 1 {
		ordered := []string{"adaptive", "enabled"}
		modes := result.ThinkingModes[:0]
		for _, mode := range ordered {
			if seenModes[mode] {
				modes = append(modes, mode)
			}
		}
		result.ThinkingModes = modes
	}
	return result
}

func hasOption(c *providers.ReasoningCapabilities, t providers.ReasoningOptionType) bool {
	if c == nil {
		return false
	}
	for _, opt := range c.Options {
		if opt.Type == t {
			return true
		}
	}
	return false
}

func findBudget(c *providers.ReasoningCapabilities) *providers.ReasoningOption {
	if c == nil {
		return nil
	}
	for i := range c.Options {
		if c.Options[i].Type == providers.ReasoningOptionBudgetTokens {
			return &c.Options[i]
		}
	}
	return nil
}

// mergeBoolPtr prefers a when both are present; otherwise returns whichever is
// non-nil. For superset semantics, if either is true, the result is true; if
// either is false and neither is true, the result is false.
func mergeBoolPtr(a, b *bool) *bool {
	if a != nil && b != nil {
		v := *a || *b
		return &v
	}
	if a != nil {
		return a
	}
	return b
}
