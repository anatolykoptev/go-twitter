// Code in this file is a verbatim copy of xtid/utils.go at 2918c81 (before the
// reference-parity fix), kept only so cmd/xtid-probe can measure the old
// algorithm (truncated hex fraction, fill-only SVG lookup) against the fixed one.
// Do not import outside cmd/xtid-probe.

package legacyxtid

import (
	"fmt"
	"math"
	"strings"
)

const (
	additionalRandomNumber = 3
	defaultKeyword         = "obfiowerehiring"
)

func jsRound(num float64) float64 {
	x := math.Floor(num)
	if (num - x) >= 0.5 {
		x = math.Ceil(num)
	}
	return math.Copysign(x, num)
}

func isOdd(num int) float64 {
	if num%2 != 0 {
		return -1.0
	}
	return 0.0
}

func floatToHex(x float64) string {
	var result []string
	quotient := int(x)
	fraction := x - float64(quotient)

	for quotient > 0 {
		remainder := quotient % 16
		if remainder > 9 {
			result = append([]string{string(rune(remainder + 87))}, result...) // lowercase hex
		} else {
			result = append([]string{fmt.Sprintf("%d", remainder)}, result...)
		}
		quotient /= 16
	}

	if fraction == 0 {
		return strings.Join(result, "")
	}

	result = append(result, ".")

	for i := 0; fraction > 0 && i < 6; i++ { // limit iterations to avoid infinite loop
		fraction *= 16
		integer := int(fraction)
		fraction -= float64(integer)

		if integer > 9 {
			result = append(result, string(rune(integer+87))) // lowercase hex
		} else {
			result = append(result, fmt.Sprintf("%d", integer))
		}
	}

	return strings.Join(result, "")
}
