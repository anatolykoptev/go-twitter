package operations

import (
	"os"
	"strings"
	"testing"
)

// TestGqlFeatures_GeneratedPreferred proves CompiledGQLFeatures() sources the generated
// baseline when generatedFeatures is non-empty, and falls back to the committed
// literal when it is empty.
func TestGqlFeatures_GeneratedPreferred(t *testing.T) {
	orig := generatedFeatures
	t.Cleanup(func() { generatedFeatures = orig })

	t.Run("generated baseline used when present", func(t *testing.T) {
		generatedFeatures = map[string]any{"only_generated_flag": true}
		got := CompiledGQLFeatures()
		if _, ok := got["only_generated_flag"]; !ok {
			t.Fatalf("expected generated flag, got %v", got)
		}
		if len(got) != 1 {
			t.Fatalf("expected only the generated set, got %d flags", len(got))
		}
	})

	t.Run("falls back to committed literal when generated empty", func(t *testing.T) {
		generatedFeatures = map[string]any{}
		got := CompiledGQLFeatures()
		want := committedFeatures()
		if len(got) != len(want) {
			t.Fatalf("fallback len = %d, want committed len %d", len(got), len(want))
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("fallback[%q] = %v, want %v", k, got[k], v)
			}
		}
	})
}

// TestGeneratedFeatures_InitialNoOp proves the committed features_gen.go baseline
// is a superset of committedFeatures() — every committed flag is present with the
// same value, so the generated layer preserves the authed-session authority. The
// generated set MAY contain additional flags discovered by gql-sync drift (new
// flags x.com ships); those are not in committedFeatures() yet and are flagged
// for an authed-capture verification via the trailing comment in features_gen.go.
//
// The original exact-equality check failed on every drift PR because gql-sync
// adds new flags — defeating the drift detection pipeline (issue #39).
func TestGeneratedFeatures_InitialNoOp(t *testing.T) {
	committed := committedFeatures()
	for k, v := range committed {
		got, ok := generatedFeatures[k]
		if !ok {
			t.Errorf("committed feature %q is missing from generatedFeatures (drift dropped a known flag)", k)
			continue
		}
		if got != v {
			t.Errorf("generatedFeatures[%q] = %v, want committed %v", k, got, v)
		}
	}
	if t.Failed() {
		t.Fatalf("generatedFeatures has %d flags, committed has %d — one or more committed flags were dropped or changed",
			len(generatedFeatures), len(committed))
	}
}

// TestGqlFeatures_ReturnsCopy proves the returned map is a fresh copy: mutating it
// (as a per-op feature override might) never corrupts the shared package var.
func TestGqlFeatures_ReturnsCopy(t *testing.T) {
	orig := generatedFeatures
	t.Cleanup(func() { generatedFeatures = orig })
	generatedFeatures = map[string]any{"flag_a": true}

	got := CompiledGQLFeatures()
	got["flag_a"] = false
	got["injected"] = true

	if generatedFeatures["flag_a"] != true {
		t.Error("mutating the returned map corrupted generatedFeatures[flag_a]")
	}
	if _, ok := generatedFeatures["injected"]; ok {
		t.Error("inserting into the returned map leaked into generatedFeatures")
	}
}

// TestQueryIDCompleteness_GeneratedOrManualAllowed asserts every operation in
// Endpoints is either covered by cmd/gql-sync's generated queryIDs map or listed
// in knownManualQueryIDs. This makes the manual-only gap fail-closed: a new op
// added to Endpoints without generated coverage or an explicit allowlist entry
// cannot ship silently.
func TestQueryIDCompleteness_GeneratedOrManualAllowed(t *testing.T) {
	endpoints := Table()
	for name := range endpoints {
		if id, ok := GeneratedQueryIDs[name]; ok && id != "" {
			continue
		}
		if reason, ok := knownManualQueryIDs[name]; ok && reason != "" {
			t.Logf("manual queryID allowed for %s: %s", name, reason)
			continue
		}
		t.Errorf("endpoint %q has no generated queryID and is not in knownManualQueryIDs; add it to generated queryIDs (via gql-sync) or document it as manual", name)
	}
	for name := range knownManualQueryIDs {
		if _, ok := endpoints[name]; !ok {
			t.Errorf("knownManualQueryIDs contains stale entry %q not present in Endpoints", name)
		}
	}
}

// TestQueryIDCompleteness_AllowlistNotGrowing asserts a manual allowlist entry
// must not also appear in GeneratedQueryIDs. If gql-sync starts extracting a
// previously manual op, the allowlist entry must be removed so the gap stays
// visible. This is cheap to run after a manual gql-sync regeneration.
func TestQueryIDCompleteness_AllowlistNotGrowing(t *testing.T) {
	for name := range knownManualQueryIDs {
		if id, ok := GeneratedQueryIDs[name]; ok && id != "" {
			t.Errorf("manual allowlist entry %q also has generated queryID %q; remove it from knownManualQueryIDs", name, id)
		}
	}
}

// --- v0.6.1 stale-mutation-queryId regression guards (2026-06-20) ---
//
// The CreateRetweet/DeleteRetweet GraphQL-path queryIDs rotate on x.com's
// ~2-4-week deploy cycle and are NOT extractable from the webpack bundles (the
// only bundle identifier is the Relay persisted-query hash, which is a different
// id and returns HTTP 422). These guards pin the live-verified values and fail
// the build if a retired stale value ever reappears.

// retiredMutationQueryIDs are queryIDs that x.com has retired and that returned
// a hard error live. They must never reappear as a committed seed.
var retiredMutationQueryIDs = map[string]string{
	"CreateRetweet (pre-v0.6.1, 404)": "ojPdsZsimiJrUGLR1sjUtA",
	"DeleteRetweet (pre-v0.6.1, 422)": "iQtK4dl5hBmXewYZuEOKVw",

	// Read-op queryIDs retired by x.com and confirmed 404 live (2026-06-20 v0.6.2
	// arc). Followers had TWO dead candidates: the pre-v0.6.2 committed seed AND
	// the fa0311/TwitterInternalAPIDocument value — both 404. The live capture
	// found the working id is a third value (9jsVJ9l2uXUIKslHvJqIhw) AND that the
	// op needs a DIFFERENT feature set (see TestFollowersFeatureSet). These guard
	// against a careless revert re-seeding a dead read-op id.
	"Followers (pre-v0.6.2 committed, 404)": "FpGYzBsUxUOecYYfso0yA",
	"Followers (fa0311 stale, 404)":         "QAV06ZzlL6dfYpN3JgTxeg",
	"Following (pre-v0.6.2 committed, 404)": "UCFedrkjMz7PeEAWCWhqFw",
	"Retweeters (pre-v0.6.2 committed)":     "0BoJlKAxoNPQUHRftlwZ2w",
}

// TestNoRetiredMutationQueryIDsInSource source-greps endpoints.go and
// queryids_gen.go for any retired stale queryID. A reappearance (e.g. a careless
// revert or a bad gql-sync emit) fails the build — the regression-guard per the
// Phase-3 forbidden-pattern rule.
func TestNoRetiredMutationQueryIDsInSource(t *testing.T) {
	for _, f := range []string{"endpoints.go", "queryids_gen.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		// Match the bare quoted id so BOTH source formats are covered: the
		// endpoints.go struct literal (ID: "x") AND the queryids_gen.go map
		// literal ("Name": "x"). The gen file is the higher-risk vector — per
		// applyGeneratedOverrides the runtime precedence is env > generated >
		// committed, so a stale id re-emitted into queryids_gen.go silently
		// overrides the live-verified committed seed.
		for label, id := range retiredMutationQueryIDs {
			if strings.Contains(string(src), `"`+id+`"`) {
				t.Errorf("%s: retired queryID %s reappeared (%s)", f, id, label)
			}
		}
	}
}
