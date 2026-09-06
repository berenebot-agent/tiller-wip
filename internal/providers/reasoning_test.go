package providers

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestExtractReasoningOptions(t *testing.T) {
	tests := []struct {
		name string
		caps *ReasoningCapabilities
		want ReasoningOptions
	}{
		{
			name: "nil capabilities",
			caps: nil,
			want: ReasoningOptions{},
		},
		{
			name: "finite effort",
			caps: &ReasoningCapabilities{Options: []ReasoningOption{{
				Type:   ReasoningOptionEffort,
				Values: []string{"low", "medium", "high"},
			}}},
			want: ReasoningOptions{
				SupportsEffort:   true,
				SupportedEfforts: []string{"low", "medium", "high"},
			},
		},
		{
			name: "unrestricted effort",
			caps: &ReasoningCapabilities{Options: []ReasoningOption{{Type: ReasoningOptionEffort}}},
			want: ReasoningOptions{SupportsEffort: true},
		},
		{
			name: "effort without none cannot disable",
			caps: &ReasoningCapabilities{Options: []ReasoningOption{{
				Type:   ReasoningOptionEffort,
				Values: []string{"minimal", "low", "high"},
			}}},
			want: ReasoningOptions{
				SupportsEffort:   true,
				SupportedEfforts: []string{"minimal", "low", "high"},
			},
		},
		{
			name: "none effort enables disable",
			caps: &ReasoningCapabilities{Options: []ReasoningOption{{
				Type:   ReasoningOptionEffort,
				Values: []string{"none", "low"},
			}}},
			want: ReasoningOptions{
				SupportsEffort:   true,
				SupportedEfforts: []string{"none", "low"},
				SupportsDisable:  true,
			},
		},
		{
			name: "toggle",
			caps: &ReasoningCapabilities{Options: []ReasoningOption{{Type: ReasoningOptionToggle}}},
			want: ReasoningOptions{SupportsToggle: true},
		},
		{
			name: "budget bounds",
			caps: &ReasoningCapabilities{Options: []ReasoningOption{
				{Type: ReasoningOptionBudgetTokens, Min: reasoningInt64Ptr(1024), Max: reasoningInt64Ptr(8192)},
				{Type: ReasoningOptionBudgetTokens, Min: reasoningInt64Ptr(256), Max: reasoningInt64Ptr(16384)},
			}},
			want: ReasoningOptions{
				SupportsBudget: true,
				BudgetMin:      reasoningInt64Ptr(256),
				BudgetMax:      reasoningInt64Ptr(16384),
			},
		},
		{
			name: "budget without bounds",
			caps: &ReasoningCapabilities{Options: []ReasoningOption{{Type: ReasoningOptionBudgetTokens}}},
			want: ReasoningOptions{SupportsBudget: true},
		},
		{
			name: "combined selectors",
			caps: &ReasoningCapabilities{
				Options: []ReasoningOption{
					{Type: ReasoningOptionEffort, Values: []string{"none", "low", "high"}},
					{Type: ReasoningOptionToggle},
					{Type: ReasoningOptionBudgetTokens, Min: reasoningInt64Ptr(512), Max: reasoningInt64Ptr(32768)},
				},
				ThinkingModes: []string{"adaptive", "enabled"},
			},
			want: ReasoningOptions{
				SupportsEffort:   true,
				SupportedEfforts: []string{"none", "low", "high"},
				SupportsDisable:  true,
				SupportsBudget:   true,
				BudgetMin:        reasoningInt64Ptr(512),
				BudgetMax:        reasoningInt64Ptr(32768),
				SupportsToggle:   true,
				SupportsAdaptive: true,
				SupportsEnabled:  true,
			},
		},
		{
			name: "adaptive thinking mode",
			caps: &ReasoningCapabilities{ThinkingModes: []string{"adaptive"}},
			want: ReasoningOptions{SupportsAdaptive: true},
		},
		{
			name: "enabled thinking mode",
			caps: &ReasoningCapabilities{ThinkingModes: []string{"enabled"}},
			want: ReasoningOptions{SupportsEnabled: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractReasoningOptions(tt.caps); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ExtractReasoningOptions() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNullableReasoningCapabilitiesRoundTrip(t *testing.T) {
	caps := &ReasoningCapabilities{
		Options: []ReasoningOption{
			{Type: ReasoningOptionEffort, Values: []string{"none", "low", "high"}},
			{Type: ReasoningOptionBudgetTokens, Min: reasoningInt64Ptr(1024), Max: reasoningInt64Ptr(16384)},
			{Type: ReasoningOptionToggle},
		},
		ThinkingModes:  []string{"adaptive", "enabled"},
		DefaultEffort:  "medium",
		Mandatory:      reasoningBoolPtr(true),
		DefaultEnabled: reasoningBoolPtr(false),
		Parameters:     []string{"reasoning", "reasoning_effort", "include_reasoning"},
	}

	wantJSON, err := json.Marshal(caps)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	stored, ok := nullableReasoningCapabilities(caps).(string)
	if !ok {
		t.Fatalf("nullableReasoningCapabilities() type = %T, want string", nullableReasoningCapabilities(caps))
	}
	if stored != string(wantJSON) {
		t.Fatalf("stored JSON = %q, want %q", stored, wantJSON)
	}

	var got ReasoningCapabilities
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatalf("json.Unmarshal(stored) error = %v", err)
	}
	if !reflect.DeepEqual(got, *caps) {
		t.Fatalf("unmarshaled capabilities = %+v, want %+v", got, *caps)
	}
}

func TestNullableReasoningCapabilitiesNilIsSQLNull(t *testing.T) {
	if got := nullableReasoningCapabilities(nil); got != nil {
		t.Fatalf("nullableReasoningCapabilities(nil) = %#v, want nil for SQL NULL", got)
	}
}

func reasoningInt64Ptr(v int64) *int64 { return &v }

func reasoningBoolPtr(v bool) *bool { return &v }
