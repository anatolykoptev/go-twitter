package xtid

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

	// Multiplying a float64 by 16 is exact, so the fraction reaches zero after at
	// most ~270 steps (1074 mantissa bits / 4 for the smallest subnormal) and the
	// loop needs no cap. The reference implementation also runs until zero; a cap
	// truncates the expansion and corrupts the animation key.
	for fraction > 0 {
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
