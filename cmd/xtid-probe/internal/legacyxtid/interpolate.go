// Code in this file is a verbatim copy of xtid/interpolate.go at 2918c81 (before the
// reference-parity fix), kept only so cmd/xtid-probe can measure the old
// algorithm (truncated hex fraction, fill-only SVG lookup) against the fixed one.
// Do not import outside cmd/xtid-probe.

package legacyxtid

func interpolate(from, to []float64, f float64) []float64 {
	out := make([]float64, len(from))
	for i := range from {
		out[i] = from[i]*(1-f) + to[i]*f
	}
	return out
}
