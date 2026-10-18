package ntgpuimage

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math/rand/v2"
	"testing"
)

var mipOptionMatrix = []MipOptions{{}, {SRGB: true}, {AlphaWeighted: true}, {SRGB: true, AlphaWeighted: true}}

// adversarialNRGBA fills an image with colors and alphas that stress rounding:
// extreme and near-half alphas, fully transparent texels with nonzero RGB, and
// whole blocks of zero alpha.
func adversarialNRGBA(rng *rand.Rand, r image.Rectangle) *image.NRGBA {
	img := image.NewNRGBA(r)
	alphas := []byte{0, 0, 1, 2, 3, 126, 127, 128, 129, 253, 254, 255, 255}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := color.NRGBA{R: byte(rng.IntN(256)), G: byte(rng.IntN(256)), B: byte(rng.IntN(256)), A: byte(rng.IntN(256))}
			switch rng.IntN(4) {
			case 0:
				c.A = alphas[rng.IntN(len(alphas))]
			case 1:
				// Zero alpha for whole 2x2 blocks, with nonzero RGB.
				if ((x-r.Min.X)/2+(y-r.Min.Y)/2)%3 == 0 {
					c.A = 0
				}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// referenceMipChain builds the chain with the general area-overlap filter only.
func referenceMipChain(src image.Image, opts MipOptions) []*image.NRGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	base := &image.NRGBA{Pix: packPixels(src), Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	levels := []*image.NRGBA{base}
	for w > 1 || h > 1 {
		w, h = max(1, w/2), max(1, h/2)
		base = downsampleMipArea(base, w, h, opts)
		levels = append(levels, base)
	}
	return levels
}

func expectSameChain(t *testing.T, name string, got []image.Image, want []*image.NRGBA) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d levels, want %d", name, len(got), len(want))
	}
	for i := range got {
		g, ok := got[i].(*image.NRGBA)
		if !ok {
			t.Fatalf("%s: level %d is %T", name, i, got[i])
		}
		if g.Rect != want[i].Rect || g.Stride != want[i].Stride {
			t.Fatalf("%s: level %d geometry %v/%d, want %v/%d", name, i, g.Rect, g.Stride, want[i].Rect, want[i].Stride)
		}
		if !bytes.Equal(g.Pix, want[i].Pix) {
			for j := range g.Pix {
				if g.Pix[j] != want[i].Pix[j] {
					t.Fatalf("%s: level %d (%dx%d) byte %d (pixel %d channel %d): got %d, want %d", name, i, g.Rect.Dx(), g.Rect.Dy(), j, j/4, j%4, g.Pix[j], want[i].Pix[j])
				}
			}
		}
	}
}

func TestMipReductionShapes(t *testing.T) {
	for _, c := range []struct {
		sw, sh, w, h, fx, fy int
		ok                   bool
	}{
		{512, 512, 256, 256, 2, 2, true},
		{2, 2, 1, 1, 2, 2, true},
		{1, 1, 1, 1, 1, 1, true},
		{1, 16, 1, 8, 1, 2, true},
		{16, 1, 8, 1, 2, 1, true},
		{1, 2, 1, 1, 1, 2, true},
		{2, 1, 1, 1, 2, 1, true},
		{8, 2, 4, 1, 2, 2, true},
		{3, 3, 1, 1, 0, 0, false},
		{5, 4, 2, 2, 0, 0, false},
		{4, 5, 2, 2, 0, 0, false},
		{1, 3, 1, 1, 0, 0, false},
		{3, 1, 1, 1, 0, 0, false},
		{6, 6, 3, 3, 2, 2, true},
		{4, 4, 1, 1, 0, 0, false},
		{3, 2, 3, 1, 0, 0, false}, // same-width non-unit axis never occurs in a chain
	} {
		fx, fy, ok := mipReduction(c.sw, c.sh, c.w, c.h)
		if fx != c.fx || fy != c.fy || ok != c.ok {
			t.Errorf("mipReduction(%d,%d -> %d,%d) = %d,%d,%v; want %d,%d,%v", c.sw, c.sh, c.w, c.h, fx, fy, ok, c.fx, c.fy, c.ok)
		}
	}
}

func TestMipChainMatchesGeneralFilter(t *testing.T) {
	sizes := []image.Point{
		{1, 1}, {1, 2}, {2, 1}, {2, 2}, {1, 17}, {17, 1}, {1, 16}, {16, 1}, {1, 3}, {3, 1},
		{4, 4}, {8, 2}, {2, 8}, {16, 16}, {64, 32}, {32, 64}, {6, 4}, {4, 6}, {12, 20},
		{3, 3}, {7, 5}, {5, 7}, {9, 16}, {100, 50}, {128, 96}, {96, 24}, {257, 128}, {255, 256},
	}
	if !testing.Short() {
		sizes = append(sizes, image.Point{512, 512}, image.Point{512, 256}, image.Point{511, 513})
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, size := range sizes {
		for _, opts := range mipOptionMatrix {
			name := fmt.Sprintf("%dx%d/%+v", size.X, size.Y, opts)
			for fixture := range 3 {
				var src *image.NRGBA
				switch fixture {
				case 0:
					src = adversarialNRGBA(rng, image.Rectangle{Max: size})
				case 1: // Opaque, like a map texture.
					src = adversarialNRGBA(rng, image.Rectangle{Max: size})
					for i := 3; i < len(src.Pix); i += 4 {
						src.Pix[i] = 255
					}
				case 2: // Nonzero bounds and a wider stride than the subimage.
					big := adversarialNRGBA(rng, image.Rect(0, 0, size.X+13, size.Y+7))
					src = big.SubImage(image.Rect(5, 3, 5+size.X, 3+size.Y)).(*image.NRGBA)
				}
				before := bytes.Clone(src.Pix)
				got, err := MipChain(src, opts)
				if err != nil {
					t.Fatal(err)
				}
				expectSameChain(t, fmt.Sprintf("%s/fixture%d", name, fixture), got, referenceMipChain(src, opts))
				if !bytes.Equal(before, src.Pix) {
					t.Fatalf("%s: source modified", name)
				}
				got[0].(*image.NRGBA).Pix[0] ^= 0xff
				if !bytes.Equal(before, src.Pix) {
					t.Fatalf("%s: level 0 aliases the source", name)
				}
			}
		}
	}
}

func TestMipChainOtherImageTypes(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	nrgba := adversarialNRGBA(rng, image.Rect(0, 0, 64, 48))
	for _, dst := range []draw.Image{
		image.NewRGBA(image.Rect(2, 2, 66, 50)),
		image.NewGray(image.Rect(-3, 1, 61, 49)),
		image.NewRGBA64(image.Rect(0, 0, 64, 48)),
		image.NewNRGBA64(image.Rect(7, 0, 71, 48)),
	} {
		draw.Draw(dst, dst.Bounds(), nrgba, nrgba.Bounds().Min, draw.Src)
		for _, opts := range mipOptionMatrix {
			got, err := MipChain(dst, opts)
			if err != nil {
				t.Fatal(err)
			}
			expectSameChain(t, fmt.Sprintf("%T/%+v", dst, opts), got, referenceMipChain(dst, opts))
		}
	}
}

func TestMipChainZeroAlphaBlocks(t *testing.T) {
	// A fully transparent 2x2 block with nonzero RGB yields zero alpha, with RGB
	// kept or dropped by AlphaWeighted; a mixed block weights color by coverage.
	src := image.NewNRGBA(image.Rect(0, 0, 4, 2))
	for x := range 2 {
		for y := range 2 {
			src.SetNRGBA(x, y, color.NRGBA{R: 200, G: 100, B: 50, A: 0})
		}
	}
	src.SetNRGBA(2, 0, color.NRGBA{R: 255, A: 0})
	src.SetNRGBA(3, 0, color.NRGBA{G: 255, A: 255})
	src.SetNRGBA(2, 1, color.NRGBA{B: 255, A: 0})
	src.SetNRGBA(3, 1, color.NRGBA{G: 255, A: 255})
	for _, opts := range mipOptionMatrix {
		got, err := MipChain(src, opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := mipReduction(4, 2, 2, 1); !ok {
			t.Fatal("4x2 -> 2x1 must use the box reduction")
		}
		expectSameChain(t, fmt.Sprintf("%+v", opts), got, referenceMipChain(src, opts))
		level := got[1].(*image.NRGBA)
		// Alpha weighting drops transparent color entirely; unweighted
		// averaging keeps it for data textures.
		want := color.NRGBA{R: 200, G: 100, B: 50}
		if opts.AlphaWeighted {
			want = color.NRGBA{}
		}
		if p := level.NRGBAAt(0, 0); p != want {
			t.Fatalf("%+v: transparent block %v, want %v", opts, p, want)
		}
		p := level.NRGBAAt(1, 0)
		if opts.AlphaWeighted && p != (color.NRGBA{G: 255, A: 128}) {
			t.Fatalf("%+v: transparent color leaked into %v", opts, p)
		}
		if !opts.AlphaWeighted && (p.A != 128 || p.R == 0 || p.B == 0 || p.G == 255) {
			t.Fatalf("%+v: unweighted averaging ignored transparent texels: %v", opts, p)
		}
	}
}
