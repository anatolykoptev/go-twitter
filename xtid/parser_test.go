package xtid

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
)

// TestGetKeyIndices_Shapes proves the anchored indicesRegex
// (`\(\w+\[(\d{1,3})\]\s*,\s*16\)`) extracts indices from the genuine
// parenthesized `(<var>[N],16)` derivation form regardless of the variable name
// length (single-char `d`, multi-char `arr`), index width (1-3 digits), and
// optional whitespace around the radix comma.
func TestGetKeyIndices_Shapes(t *testing.T) {
	cases := []struct {
		name        string
		js          string
		wantRow     int
		wantIndices []int
	}{
		{
			// Single-char variable, whitespace after the comma.
			name:        "single-char var with spacing",
			js:          `c=function(d){return o(d[5], 16)*o(d[9], 16)}`,
			wantRow:     5,
			wantIndices: []int{9},
		},
		{
			// Multi-char variable prefix, 3-digit index ([127]).
			name:        "multi-char var with 3-digit index",
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

// realBundleRowIndex / realBundleKeyIndices are the values derived by reading the
// real ondemand.s captured 2026-06-19 (testdata/ondemand_real.js, served as
// ondemand.s.91612d9a.js). The genuine derivation is
//
//	let[C,G]=[o[l(...)](n[27],16),
//	          o[l(...)](o[f(...)](o[i(...)](n[47],16), o[l(...)](n[47],16)),
//	                    o[i(...)](n[42],16))]
//
// so the indices in document order are 27,47,47,42 -> rowIndex 27, key-byte
// indices [47,47,42]. n[47] genuinely appears twice (once per branch of the
// o[f(...)] call), so the duplicate is intentional, not an artifact.
const realBundleRowIndex = 27

var realBundleKeyIndices = []int{47, 47, 42}

func readRealBundle(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ondemand_real.js"))
	if err != nil {
		t.Fatalf("read real ondemand.s fixture: %v", err)
	}
	return string(b)
}

// TestGetKeyIndices_RealBundle is the decisive gate: it runs getKeyIndices against
// the REAL captured ondemand.s and asserts the exact rowIndex + key-byte indices.
// Any indicesRegex regression that over- or under-matches changes these values and
// fails here — a well-formed but WRONG x-client-transaction-id would otherwise pass
// every synthetic test while Twitter silently 404s.
func TestGetKeyIndices_RealBundle(t *testing.T) {
	row, indices := getKeyIndices(readRealBundle(t))
	if row != realBundleRowIndex {
		t.Fatalf("rowIndex = %d want %d", row, realBundleRowIndex)
	}
	if !reflect.DeepEqual(indices, realBundleKeyIndices) {
		t.Fatalf("keyBytesIndices = %v want %v", indices, realBundleKeyIndices)
	}
}

// TestGetKeyIndices_RealBundleNoOverMatch proves the anchored regex does not let
// stray `[N],16` noise pollute the index list. The pre-fix loosened regex
// (`\[(\d+)\],\s*16`) matched every one of these fragments; prepended to the real
// bundle they would have shifted rowIndex to 99 and injected 88/1234 into the
// key-byte indices, yielding a well-formed but WRONG transaction id. The anchored
// regex rejects all of them, so the extracted indices stay identical to the clean
// fixture.
func TestGetKeyIndices_RealBundleNoOverMatch(t *testing.T) {
	// Noise the genuine derivation never produces:
	//   f([99],16)  - array-literal arg, no key-bytes variable before `[`
	//   [88],16     - bare bracket access, no wrapping call
	//   q[1234],16  - 4-digit value, never a real key-byte position
	const noise = `;var _a=f([99],16),_b=[88],16,_c=q[1234],16;`

	// Guard: confirm the noise is a genuine discriminator — the pre-fix loosened
	// regex DID match all three fragments, so the anchoring is what protects us.
	loosened := regexp.MustCompile(`\[(\d+)\],\s*16`)
	if got := loosened.FindAllString(noise, -1); len(got) != 3 {
		t.Fatalf("expected loosened regex to match 3 noise fragments, got %d: %v", len(got), got)
	}

	row, indices := getKeyIndices(noise + readRealBundle(t))
	if row != realBundleRowIndex {
		t.Fatalf("rowIndex polluted by noise: got %d want %d", row, realBundleRowIndex)
	}
	if !reflect.DeepEqual(indices, realBundleKeyIndices) {
		t.Fatalf("keyBytesIndices polluted by noise: got %v want %v", indices, realBundleKeyIndices)
	}
	for _, bad := range []int{99, 88, 1234} {
		if row == bad {
			t.Fatalf("noise index %d leaked into rowIndex", bad)
		}
		for _, got := range indices {
			if got == bad {
				t.Fatalf("noise index %d leaked into keyBytesIndices %v", bad, indices)
			}
		}
	}
}
