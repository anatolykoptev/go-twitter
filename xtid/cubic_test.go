package xtid

import (
	"hash/fnv"
	"math"
	"math/rand/v2"
	"testing"
)

// The solver's output must not depend on the architecture: arm64 fuses a
// multiply and an add into one rounding unless every product is converted to
// float64 first, amd64 does not, and without the conversions the two differ in
// the last digits. The expected hash is of 1M cases (random curves and times),
// recorded on arm64 and on amd64 under Rosetta, which agreed. It is not
// Chromium's bit pattern; TestBrowserGolden holds the keys to the browser.
func TestCubicArchIndependent(t *testing.T) {
	const want = "d338f736f26d240d"
	r := rand.New(rand.NewPCG(9, 9))
	h := fnv.New64a()
	ct := &ClientTransaction{}
	buf := make([]byte, 8)
	put := func(f float64) {
		b := math.Float64bits(f)
		for i := range buf {
			buf[i] = byte(b >> (8 * i))
		}
		h.Write(buf)
	}
	for n := 0; n < 1_000_000; n++ {
		curves := make([]float64, 4)
		for i := range curves {
			curves[i] = ct.solve(float64(r.IntN(256)), isOdd(i), 1.0, false)
		}
		c := newCubic(curves)
		tt := float64(r.IntN(338)*10) / 4096
		if n%4 == 0 {
			tt = r.Float64()
		}
		put(c.getValue(tt))
		put(c.solveCurveX(tt, cubicEpsilon))
		put(c.sampleDerivativeX(tt))
	}
	if got := hex64(h.Sum64()); got != want {
		t.Fatalf("solver hash = %s, want %s", got, want)
	}
}

func hex64(v uint64) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		out[i] = digits[v&0xf]
		v >>= 4
	}
	return string(out)
}
