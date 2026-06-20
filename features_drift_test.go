package twitter

import "testing"

// featureDriftAllowlist is an explicit escape hatch: a flag listed here is
// allowed to differ between committedFeatures() and generatedFeatures() without
// failing TestFeatureDrift_CommittedVsGenerated. It MUST stay empty by default —
// a populated entry is a conscious, reviewed exception and is printed by the
// test so the divergence is never invisible.
var featureDriftAllowlist = map[string]struct{}{}

// TestFeatureDrift_CommittedVsGenerated turns a silent feature-value flip into a
// loud CI failure. committedFeatures() is the authed-session authority; for every
// flag present in BOTH committedFeatures() and the generated baseline, the values
// MUST be equal. A generated value that differs would ship a wrong wire value
// (HTTP 400 on every authenticated call) — exactly the bug the warm-page-guest
// default extraction risked. Names present in only one map are legitimate
// adds/removes and are out of scope here (the // REMOVED block + NEW comments
// cover those).
func TestFeatureDrift_CommittedVsGenerated(t *testing.T) {
	committed := committedFeatures()
	for name, gen := range generatedFeatures {
		com, ok := committed[name]
		if !ok {
			continue // generated-only (NEW flag) — not a drift, handled by review comment
		}
		if _, allowed := featureDriftAllowlist[name]; allowed {
			t.Logf("DRIFT ALLOWED (escape hatch): %q committed=%v generated=%v", name, com, gen)
			continue
		}
		if com != gen {
			t.Errorf("DRIFT: flag %q committed=%v generated=%v — committed values are "+
				"authoritative for known flags; a generated value that differs ships a "+
				"wrong wire value (HTTP 400 risk). Regenerate via gql-sync (committed-wins) "+
				"or update committedFeatures().", name, com, gen)
		}
	}
}
