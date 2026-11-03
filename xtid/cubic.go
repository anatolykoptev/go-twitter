package xtid

import "math"

// Cubic is the easing curve X's loading animation applies, a CSS
// cubic-bezier(x1, y1, x2, y2) from curves = [x1, y1, x2, y2]. X runs it in the
// browser, so the value is whatever Chromium computes, and this is a port of
// gfx::CubicBezier (ui/gfx/geometry/cubic_bezier.cc, ui/gfx/geometry/cubic_bezier.h):
// a piecewise-linear first guess from 11 samples of x(t), up to
// cubicNewtonIterations Newton steps, then bisection inside the sample bracket.
// The Lqm1 reference replaces all of it with plain bisection to 1e-5, which
// returns a different t for ~0.3% of keys and moves a printed digit.
// testdata/browser holds the browser's own answers.
type Cubic struct {
	ax, bx, cx, ay, by, cy float64
	startGradient          float64
	endGradient            float64
	splineSamples          [cubicSplineSamples]float64
}

const (
	// cubicEpsilon is gfx::CubicBezier's kBezierEpsilon, the tolerance
	// CubicBezier::Solve uses.
	cubicEpsilon = 1e-7
	// cubicNewtonIterations is its kMaxNewtonIterations.
	cubicNewtonIterations = 4
	// cubicSplineSamples is its CUBIC_BEZIER_SPLINE_SAMPLES.
	cubicSplineSamples = 11
)

// newCubic builds the curve from [x1, y1, x2, y2] (InitCoefficients,
// InitGradients and InitSpline of gfx::CubicBezier).
func newCubic(curves []float64) *Cubic {
	x1, y1, x2, y2 := curves[0], curves[1], curves[2], curves[3]
	c := &Cubic{}
	c.cx = 3.0 * x1
	c.bx = 3.0*(x2-x1) - c.cx
	c.ax = 1.0 - c.cx - c.bx
	c.cy = 3.0 * y1
	c.by = 3.0*(y2-y1) - c.cy
	c.ay = 1.0 - c.cy - c.by

	switch {
	case x1 > 0:
		c.startGradient = y1 / x1
	case y1 == 0 && x2 > 0:
		c.startGradient = y2 / x2
	case y1 == 0 && y2 == 0:
		c.startGradient = 1
	}
	switch {
	case x2 < 1:
		c.endGradient = (y2 - 1) / (x2 - 1)
	case y2 == 1 && x1 < 1:
		c.endGradient = (y1 - 1) / (x1 - 1)
	case y2 == 1 && y1 == 1:
		c.endGradient = 1
	}

	deltaT := 1.0 / (cubicSplineSamples - 1)
	for i := range c.splineSamples {
		c.splineSamples[i] = c.sampleX(float64(i) * deltaT)
	}
	return c
}

// sampleX and sampleY evaluate ((a*t + b)*t + c)*t. Every product is wrapped in
// a float64 conversion so no compiler fuses it into a multiply-add (see #131):
// the result is the same on every architecture.
func (c *Cubic) sampleX(t float64) float64 {
	return float64(float64(float64(c.ax*t)+c.bx)*t+c.cx) * t
}

func (c *Cubic) sampleY(t float64) float64 {
	return float64(float64(float64(c.ay*t)+c.by)*t+c.cy) * t
}

func (c *Cubic) sampleDerivativeX(t float64) float64 {
	return float64(float64(float64(3.0*c.ax)*t)+float64(2.0*c.bx))*t + c.cx
}

// solveCurveX finds t with sampleX(t) ~= x (gfx::CubicBezier::SolveCurveX).
func (c *Cubic) solveCurveX(x, epsilon float64) float64 {
	t0, t1, t2 := 0.0, 1.0, x
	deltaT := 1.0 / (cubicSplineSamples - 1)
	for i := 1; i < cubicSplineSamples; i++ {
		if x <= c.splineSamples[i] {
			t1 = deltaT * float64(i)
			t0 = t1 - deltaT
			t2 = t0 + float64((t1-t0)*(x-c.splineSamples[i-1]))/(c.splineSamples[i]-c.splineSamples[i-1])
			break
		}
	}

	// Newton's method. x2 keeps the residual of the last point evaluated, which
	// is the one before the final step: Chromium tests that stale value too.
	var x2 float64
	newtonEpsilon := math.Min(cubicEpsilon, epsilon)
	for i := 0; i < cubicNewtonIterations; i++ {
		x2 = c.sampleX(t2) - x
		if math.Abs(x2) < newtonEpsilon {
			return t2
		}
		d2 := c.sampleDerivativeX(t2)
		if math.Abs(d2) < cubicEpsilon {
			break
		}
		t2 -= x2 / d2
	}
	if math.Abs(x2) < epsilon {
		return t2
	}

	// Bisection, for reliability.
	for t0 < t1 {
		x2 = c.sampleX(t2)
		if math.Abs(x2-x) < epsilon {
			return t2
		}
		if x > x2 {
			t0 = t2
		} else {
			t1 = t2
		}
		t2 = (t1 + t0) * 0.5
	}
	return t2
}

// getValue is gfx::CubicBezier::Solve: the eased value at progress t, extended
// linearly by the end gradients outside [0, 1].
func (c *Cubic) getValue(t float64) float64 {
	if t < 0 {
		return 0.0 + float64(c.startGradient*t)
	}
	if t > 1 {
		return 1.0 + float64(c.endGradient*(t-1.0))
	}
	return c.sampleY(c.solveCurveX(t, cubicEpsilon))
}
