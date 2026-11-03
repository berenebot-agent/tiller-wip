package store

import (
	"database/sql"
	"testing"
)

// TestCachePctUsesWholePrompt pins the cache-share formula. input is the whole
// prompt (normalised at ingestion), so the share is cache_read/input. There is
// deliberately no upper clamp: a row that predates the normalisation and could
// not be identified keeps its stored value rather than being masked.
func TestCachePctUsesWholePrompt(t *testing.T) {
	t.Parallel()
	valid := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	cases := []struct {
		name string
		read sql.NullFloat64
		in   sql.NullFloat64
		want *float64
	}{
		{"ninety of one hundred", valid(90), valid(100), floatPtr(90)},
		{"fully cached", valid(100), valid(100), floatPtr(100)},
		{"no cache reads", valid(0), valid(100), floatPtr(0)},
		{"no prompt", valid(0), valid(0), nil},
		{"no cache report", sql.NullFloat64{}, valid(100), nil},
		{"no input report", valid(90), sql.NullFloat64{}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := cachePct(tc.read, tc.in)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("cachePct() = %v, want %v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Fatalf("cachePct() = %v, want %v", *got, *tc.want)
			}
		})
	}
}

func floatPtr(v float64) *float64 { return &v }
