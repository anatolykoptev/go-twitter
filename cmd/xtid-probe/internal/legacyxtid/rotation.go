// Code in this file is a verbatim copy of xtid/rotation.go at 2918c81 (before the
// reference-parity fix), kept only so cmd/xtid-probe can measure the old
// algorithm (truncated hex fraction, fill-only SVG lookup) against the fixed one.
// Do not import outside cmd/xtid-probe.

package legacyxtid

import "math"

func convertRotationToMatrix(degrees float64) []float64 {
	rad := degrees * math.Pi / 180
	return []float64{math.Cos(rad), -math.Sin(rad), math.Sin(rad), math.Cos(rad)}
}
