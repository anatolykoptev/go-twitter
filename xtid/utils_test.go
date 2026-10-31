package xtid

import (
	"math"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// oracleHex is an independent expansion of x >= 0 in the format floatToHex
// produces: the lowercase hex integer part (empty when 0), then "." and the
// exact hex fraction without trailing zeros (omitted when the fraction is 0).
// It is derived from strconv's exact 'x' formatting (mantissa and binary
// exponent) with big.Int shifts, not from floatToHex's multiply-by-16 loop.
func oracleHex(t *testing.T, x float64) string {
	t.Helper()
	// 'x' form: 0x1.<hex mantissa>p<exp>, or 0x0p+00 for zero.
	s := strconv.FormatFloat(x, 'x', -1, 64)
	s = strings.TrimPrefix(s, "0x")
	mant, expStr, ok := strings.Cut(s, "p")
	if !ok {
		t.Fatalf("unexpected 'x' format %q", s)
	}
	exp, err := strconv.Atoi(expStr)
	if err != nil {
		t.Fatalf("bad exponent in %q: %v", s, err)
	}
	lead, frac, _ := strings.Cut(mant, ".")
	n, okN := new(big.Int).SetString(lead+frac, 16) // mantissa as an integer
	if !okN {
		t.Fatalf("bad mantissa in %q", s)
	}
	// x = n * 2^(exp - 4*len(frac)).
	shift := exp - 4*len(frac)
	if n.Sign() == 0 {
		return ""
	}
	var intPart, fracBits *big.Int
	var k int // fracBits / 2^k is the fractional part
	if shift >= 0 {
		intPart = new(big.Int).Lsh(n, uint(shift))
		fracBits = new(big.Int)
	} else {
		k = -shift
		intPart = new(big.Int).Rsh(n, uint(k))
		fracBits = new(big.Int).Sub(n, new(big.Int).Lsh(intPart, uint(k)))
	}
	out := ""
	if intPart.Sign() != 0 {
		out = intPart.Text(16)
	}
	if fracBits.Sign() == 0 {
		return out
	}
	digits := (k + 3) / 4 // pad the denominator to a whole number of hex digits
	fracBits.Lsh(fracBits, uint(4*digits-k))
	f := fracBits.Text(16)
	f = strings.Repeat("0", digits-len(f)) + f
	return out + "." + strings.TrimRight(f, "0")
}

func TestOracleHex_KnownAnswers(t *testing.T) {
	// Hand-checked against float64 bit patterns: 0.5 = 0x1p-1, 1.75 = 0x1.cp+0.
	for in, want := range map[float64]string{0.5: ".8", 0.25: ".4", 1.75: "1.c", 16: "10", 0: "", 255.5: "ff.8"} {
		if got := oracleHex(t, in); got != want {
			t.Errorf("oracleHex(%v) = %q, want %q", in, got, want)
		}
	}
}

// Real animation keys carry the full hex expansion of the rounded matrix
// values. 0.16 and 0.8 are not exactly representable, so their fractions run
// for 13-14 digits; the reference loops until the fraction is zero.
func TestFloatToHex_KnownAnswers(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.16, ".28f5c28f5c28f6"},
		{0.8, ".ccccccccccccd"},
		{0.5, ".8"},
		{1, "1"},
		{0, ""},
		{10.5, "a.8"},
	}
	for _, tc := range cases {
		if got := floatToHex(tc.in); got != tc.want {
			t.Errorf("floatToHex(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFloatToHex_MatchesExactExpansion(t *testing.T) {
	var vals []float64
	// Every value the matrix rounding (round(v*100)/100, sign dropped) can
	// produce in [-2, 2].
	for k := 0; k <= 200; k++ {
		vals = append(vals, math.Round(float64(k))/100)
	}
	// Random fractions across several magnitudes.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		vals = append(vals, rng.Float64()*math.Pow(16, float64(rng.Intn(4))))
	}
	for i := 0; i < 500; i++ { // tiny values force the longest expansions
		vals = append(vals, rng.Float64()*math.Pow(2, -float64(rng.Intn(60))))
	}
	for _, v := range vals {
		if got, want := floatToHex(v), oracleHex(t, v); got != want {
			t.Errorf("floatToHex(%v) = %q, want %q", v, got, want)
		}
	}
	t.Logf("checked %d values", len(vals))
}
