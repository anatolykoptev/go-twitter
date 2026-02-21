package xtid

import "math"

func convertRotationToMatrix(degrees float64) []float64 {
	rad := degrees * math.Pi / 180
	return []float64{math.Cos(rad), -math.Sin(rad), math.Sin(rad), math.Cos(rad)}
}
