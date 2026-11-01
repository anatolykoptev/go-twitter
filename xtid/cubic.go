package xtid

import "math"

type Cubic struct {
	curves []float64
}

func newCubic(curves []float64) *Cubic {
	return &Cubic{curves: curves}
}

func (c *Cubic) getValue(t float64) float64 {
	var startGradient float64
	var endGradient float64
	start := 0.0
	mid := 0.0
	end := 1.0

	if t <= 0.0 {
		if c.curves[0] > 0.0 {
			startGradient = c.curves[1] / c.curves[0]
		} else if c.curves[1] == 0.0 && c.curves[2] > 0.0 {
			startGradient = c.curves[3] / c.curves[2]
		}
		return startGradient * t
	}

	if t >= 1.0 {
		if c.curves[2] < 1.0 {
			endGradient = (c.curves[3] - 1.0) / (c.curves[2] - 1.0)
		} else if c.curves[2] == 1.0 && c.curves[0] < 1.0 {
			endGradient = (c.curves[1] - 1.0) / (c.curves[0] - 1.0)
		}
		return 1.0 + float64(endGradient*(t-1.0)) // explicit conversion: no fused multiply-add
	}

	for start < end {
		mid = (start + end) / 2
		xEst := cubicCalc(c.curves[0], c.curves[2], mid)
		if math.Abs(t-xEst) < cubicEpsilon {
			return cubicCalc(c.curves[1], c.curves[3], mid)
		}
		if xEst < t {
			start = mid
		} else {
			end = mid
		}
	}
	return cubicCalc(c.curves[1], c.curves[3], mid)
}

// cubicEpsilon is the bisection tolerance of the reference (`< 0.00001`). A
// tighter value picks a different bisection step for ~0.3% of key bytes, which
// moves a rounded channel or matrix digit.
const cubicEpsilon = 0.00001

// cubicCalc evaluates the cubic Bezier polynomial. The Go spec lets a compiler
// fuse x*y + z into one fused multiply-add instruction without rounding the
// product (arm64 does, amd64 does not), whereas JavaScript always rounds every
// operation. Each product is therefore wrapped in an explicit float64
// conversion, which forbids fusion, so the result is bit-identical on every
// architecture and equal to the reference.
func cubicCalc(a, b, m float64) float64 {
	t1 := float64(3.0 * a * (1 - m) * (1 - m) * m)
	t2 := float64(3.0 * b * (1 - m) * m * m)
	t3 := float64(m * m * m)
	return t1 + t2 + t3
}
