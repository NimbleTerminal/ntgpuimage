package ntgpuimage

import (
	"math"
	"math/rand/v2"
	"testing"
)

func expectSameSRGB(t *testing.T, v float64) {
	t.Helper()
	if got, want := encodeSRGB(v), encodeSRGBExact(v); got != want {
		t.Fatalf("encodeSRGB(%.17g) = %d, exact %d", v, got, want)
	}
}

func TestEncodeSRGBMatchesExactAtBoundaries(t *testing.T) {
	// Every rounding boundary, the piecewise-function junction, and the guard
	// band edges, stepped through neighbouring float64 values.
	points := append([]float64{0.0031308, 0, 1, 0.04045 / 12.92}, srgbThreshold[:]...)
	for _, p := range points {
		for _, d := range []float64{0, srgbGuard * 0.5, srgbGuard * 0.999, srgbGuard, srgbGuard * 1.001, srgbGuard * 2, 1e-7, 1e-6} {
			for _, v := range []float64{p - d, p + d} {
				x := v
				for range 48 {
					expectSameSRGB(t, x)
					x = math.Nextafter(x, 2)
				}
				x = v
				for range 48 {
					expectSameSRGB(t, x)
					x = math.Nextafter(x, -1)
				}
			}
		}
	}
	for _, v := range []float64{-1, -1e-300, 1e-300, 1.5, 2, 1 + 1e-9, math.Copysign(0, -1), math.MaxFloat64, math.SmallestNonzeroFloat64} {
		expectSameSRGB(t, v)
	}
	// Out-of-range and NaN inputs take the exact path, keeping its behavior.
	if encodeSRGB(math.NaN()) != encodeSRGBExact(math.NaN()) || encodeSRGB(math.Inf(1)) != encodeSRGBExact(math.Inf(1)) {
		t.Fatal("non-finite handling differs")
	}
}

func TestEncodeSRGBMatchesExactRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	n := 2_000_000
	if testing.Short() {
		n = 200_000
	}
	for range n {
		expectSameSRGB(t, rng.Float64())
		// Values the filters actually produce: averages of linearized bytes.
		a, b, c, d := srgbLinear[rng.IntN(256)], srgbLinear[rng.IntN(256)], srgbLinear[rng.IntN(256)], srgbLinear[rng.IntN(256)]
		expectSameSRGB(t, (a+b+c+d)/4)
		// Alpha-weighted averages with arbitrary coverage.
		wa, wb := unitFloat[rng.IntN(256)], unitFloat[rng.IntN(256)]
		if w := wa + wb; w > 0 {
			expectSameSRGB(t, (wa*a+wb*b)/w)
		}
	}
	// Every two-tap opaque average, as produced in the 1xN/Nx1 tail.
	for a := range 256 {
		for b := range 256 {
			expectSameSRGB(t, (srgbLinear[a]+srgbLinear[b])/2)
		}
	}
}

func BenchmarkEncodeSRGB(b *testing.B) {
	rng := rand.New(rand.NewPCG(7, 8))
	values := make([]float64, 4096)
	for i := range values {
		values[i] = (srgbLinear[rng.IntN(256)] + srgbLinear[rng.IntN(256)] + srgbLinear[rng.IntN(256)] + srgbLinear[rng.IntN(256)]) / 4
	}
	var sink byte
	b.Run("exact", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			sink += encodeSRGBExact(values[i&4095])
		}
	})
	b.Run("table", func(b *testing.B) {
		for i := 0; b.Loop(); i++ {
			sink += encodeSRGB(values[i&4095])
		}
	})
	_ = sink
}
