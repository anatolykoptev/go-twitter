package xtid

import (
	"math"
	"reflect"
	"testing"
)

// Expected values were computed by V8, the JavaScript engine X's script runs
// on: Number(x.toFixed(2)).
func TestToFixed2(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{
		{0.125, 0.13},
		{0.375, 0.38},
		{0.625, 0.63},
		{0.875, 0.88},
		{1.125, 1.13},
		{-0.125, -0.13},
		{-0.375, -0.38},
		{0.695, 0.69},
		{-0.695, -0.69},
		{1.005, 1},
		{8.345, 8.35},
		{0.005, 0.01},
		{0.015, 0.01},
		{0.025, 0.03},
		{0.035, 0.04},
		{2.675, 2.67},
		{1.45, 1.45},
		{0.1, 0.1},
		{0.3, 0.3},
		{0.719009, 0.72},
		{9.67769, 9.68},
		{5.68575, 5.69},
		{-5, -5},
		{0, 0},
		{0, 0},
		{7.5, 7.5},
		{0.5, 0.5},
		{12.375, 12.38},
		{0.995, 0.99},
		{0.994999, 0.99},
		{123456789012345.125, 123456789012345.12},
		{98765432109876.625, 98765432109876.62},
		{562949953421312.375, 562949953421312.4},
		{0.001, 0},
		{0.875, 0.88},
		{9.995, 9.99},
		{0.999, 1},
		{19.875, 19.88},
		{99.875, 99.88},
		{-0.001, 0},
	} {
		if got := toFixed2(tc.in); got != tc.want || (tc.want == 0 && math.Signbit(got)) {
			t.Errorf("toFixed2(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The numbers X's script reads from one matrix entry as Chromium prints it
// (six significant digits, exponent form below 1e-4) split by /[\d.-]+/g.
func TestMatrixNumbers(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want []float64
	}{
		{1, []float64{1}},
		{0, []float64{0}},
		{0.7190094, []float64{0.719009}},
		{-0.69499999999, []float64{-0.695}},
		{5.68575e-05, []float64{5.68575, -5}}, // "5.68575e-05": the exponent is a second number
		{-9.67769e-06, []float64{-9.67769, -6}},
		{0.0001234567, []float64{0.000123457}},
		{math.Copysign(0, -1), []float64{0}},
	} {
		if got := matrixNumbers(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("matrixNumbers(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
