package ntgpuimage

import (
	"fmt"
	"image"
	"image/color"
	"testing"
)

// benchMipImage is a deterministic opaque map-like image: blocks, streets, and
// a river, matching the ntcharts-osm mapview3d benchmark texture.
func benchMipImage(w, h int, translucent bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.NRGBA{R: 218, G: 226, B: 206, A: 255}
			switch {
			case (x+y/3)%w < w/12:
				c = color.NRGBA{R: 120, G: 175, B: 210, A: 255}
			case x%32 < 3 || y%32 < 3:
				c = color.NRGBA{R: 245, G: 240, B: 225, A: 255}
			default:
				c.R -= uint8((x/32+y/32)%5) * 8
			}
			if translucent {
				c.A = uint8((x*7 + y*13) % 256)
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func BenchmarkMipChain(b *testing.B) {
	sizes := []image.Point{{256, 256}, {512, 512}, {1024, 1024}, {512, 256}, {511, 511}, {513, 257}}
	options := []struct {
		name string
		opts MipOptions
	}{
		{"linear", MipOptions{}},
		{"srgb", MipOptions{SRGB: true}},
		{"alpha", MipOptions{AlphaWeighted: true}},
		{"srgb+alpha", MipOptions{SRGB: true, AlphaWeighted: true}},
	}
	for _, size := range sizes {
		for _, o := range options {
			if o.opts != (MipOptions{SRGB: true, AlphaWeighted: true}) && size != (image.Point{512, 512}) {
				continue // Full option matrix only at the consumer's 512x512 size.
			}
			src := benchMipImage(size.X, size.Y, false)
			b.Run(fmt.Sprintf("%dx%d/%s", size.X, size.Y, o.name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if _, err := MipChain(src, o.opts); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
	src := benchMipImage(512, 512, true)
	b.Run("512x512/translucent/srgb+alpha", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := MipChain(src, MipOptions{SRGB: true, AlphaWeighted: true}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
