package xtid

import (
	"reflect"
	"testing"
)

// TestGetKeyIndices_BothShapes proves the loosened indicesRegex (`\[(\d+)\],\s*16`)
// matches both the pre-Mar-2026 wrapped `(x[N], 16)` form and the post-Mar-2026
// bare `[N],16` form, including 3+ digit indices the old `\d{1,2}` capped form
// would have missed.
func TestGetKeyIndices_BothShapes(t *testing.T) {
	cases := []struct {
		name        string
		js          string
		wantRow     int
		wantIndices []int
	}{
		{
			// Pre-Mar-2026: wrapped in (x[N], 16) with a space.
			name:        "pre-2026 wrapped form",
			js:          `c=function(d){return o(d[5], 16)*o(d[9], 16)}`,
			wantRow:     5,
			wantIndices: []int{9},
		},
		{
			// Post-Mar-2026: bare bracket access, multi-char prefix, 3-digit index
			// ([127]) that the old `\d{1,2}` regex could not capture.
			name:        "post-2026 bare form with 3-digit index",
			js:          `u=function(arr){return f(arr[3],16)^f(arr[127],16)}`,
			wantRow:     3,
			wantIndices: []int{127},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, indices := getKeyIndices(tc.js)
			if row != tc.wantRow {
				t.Fatalf("rowIndex = %d want %d", row, tc.wantRow)
			}
			if !reflect.DeepEqual(indices, tc.wantIndices) {
				t.Fatalf("indices = %v want %v", indices, tc.wantIndices)
			}
		})
	}
}

func TestGetKeyIndices_NoMatch(t *testing.T) {
	if row, idx := getKeyIndices("no indices here"); row != 0 || idx != nil {
		t.Fatalf("getKeyIndices(no match) = %d,%v want 0,nil", row, idx)
	}
}

func TestOnDemandLegacyURL(t *testing.T) {
	const html = `<html><head><script>e={"ondemand.s":"deadbe0"}</script></head></html>`
	const want = "https://abs.twimg.com/responsive-web/client-web/ondemand.s.deadbe0a.js"

	if got := onDemandLegacyURL(html); got != want {
		t.Fatalf("onDemandLegacyURL = %q want %q", got, want)
	}
}

func TestOnDemandLegacyURL_AbsentReturnsEmpty(t *testing.T) {
	// Modern chunk-map snapshot: no direct embed -> "" so Initialize falls back to
	// the bundle core.
	const html = `<html><script>var n={20113:"ondemand.s"};var h={20113:"abc1234"};</script></html>`
	if got := onDemandLegacyURL(html); got != "" {
		t.Fatalf("onDemandLegacyURL(chunk-map only) = %q want \"\"", got)
	}
}
