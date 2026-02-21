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
		return 1.0 + endGradient*(t-1.0)
	}

	for start < end {
		mid = (start + end) / 2
		xEst := cubicCalc(c.curves[0], c.curves[2], mid)
		if math.Abs(t-xEst) < 0.00001 {
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

func cubicCalc(a, b, m float64) float64 {
	return 3.0*a*(1-m)*(1-m)*m + 3.0*b*(1-m)*m*m + m*m*m
}
