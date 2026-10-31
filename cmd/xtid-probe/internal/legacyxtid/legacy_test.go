package legacyxtid

import "testing"

// The copy must keep the old behaviour, otherwise variant a of the probe would
// measure the fixed algorithm twice.
func TestLegacyKeepsTruncatedHexFraction(t *testing.T) {
	if got := floatToHex(0.16); got != ".28f5c2" {
		t.Fatalf("floatToHex(0.16) = %q, want the truncated .28f5c2", got)
	}
}
