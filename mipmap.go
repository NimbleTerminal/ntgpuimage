package ntgpuimage

import (
	"image"
	"math"
	"math/bits"
)

// MipOptions controls CPU mip generation. No alpha-test threshold is assumed;
// callers can adjust each returned level's alpha to preserve cutout coverage.
type MipOptions struct {
	// SRGB decodes RGB before averaging and encodes it afterward. Alpha is linear.
	SRGB bool
	// AlphaWeighted averages RGB weighted by alpha, then returns straight-alpha
	// pixels. Use it for color images with transparency to avoid dark fringes.
	// Leave it false for data textures whose channels should average independently.
	AlphaWeighted bool
}

// MipChain returns independent, mutable *image.NRGBA images (as image.Image)
// including a copy of level 0 and every level down to 1x1. Dimensions halve,
// rounding down and clamping each axis to 1. Area-weighted box filtering includes
// every source texel, including the last row and column of odd-sized images.
// Pass the result to UploadTextureLevels to share exactly this chain with a CPU
// renderer. The source is not retained or modified.
func MipChain(src image.Image, opts MipOptions) ([]image.Image, error) {
	w, h, err := imageSize("generate mipmaps", src)
	if err != nil {
		return nil, err
	}
	base := &image.NRGBA{Pix: packPixels(src), Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	levels := make([]image.Image, 1, mipLevelCount(w, h))
	levels[0] = base
	for w > 1 || h > 1 {
		w, h = max(1, w/2), max(1, h/2)
		base = downsampleMip(base, w, h, opts)
		levels = append(levels, base)
	}
	return levels, nil
}

func mipLevelCount(w, h int) int { return bits.Len(uint(max(w, h))) }

var srgbLinear = func() (table [256]float64) {
	for i := range table {
		v := float64(i) / 255
		if v <= 0.04045 {
			table[i] = v / 12.92
		} else {
			table[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return
}()

func unitByte(v float64) byte { return byte(math.Round(min(1, max(0, v)) * 255)) }
func encodeSRGB(v float64) byte {
	if v <= 0.0031308 {
		return unitByte(v * 12.92)
	}
	return unitByte(1.055*math.Pow(v, 1/2.4) - 0.055)
}

// unitFloat[i] is float64(i)/255, the exact value the filters divide out of
// each 8-bit channel; a table avoids repeating the division per tap.
var unitFloat = func() (table [256]float64) {
	for i := range table {
		table[i] = float64(i) / 255
	}
	return
}()

// downsampleMip reduces one chain level (origin 0,0, tight stride) to w×h.
// Shapes where every destination texel covers whole source texels use the box
// reduction, which reproduces the general area filter byte for byte; odd and
// other mixed shapes use the general filter.
func downsampleMip(src *image.NRGBA, w, h int, opts MipOptions) *image.NRGBA {
	if fx, fy, ok := mipReduction(src.Rect.Dx(), src.Rect.Dy(), w, h); ok {
		return downsampleMipBox(src, w, h, fx, fy, opts)
	}
	return downsampleMipArea(src, w, h, opts)
}

// mipReduction reports the per-axis factor (1 or 2) when each destination texel
// covers exactly fx×fy whole source texels: an axis either halves exactly or has
// already reached 1, as in the 1×N and N×1 tail of a chain. Odd axes above 1
// need fractional coverage and report false.
func mipReduction(sw, sh, w, h int) (fx, fy int, ok bool) {
	axis := func(s, d int) (int, bool) {
		switch {
		case s == 2*d:
			return 2, true
		case s == 1 && d == 1:
			return 1, true
		}
		return 0, false
	}
	fx, okx := axis(sw, w)
	fy, oky := axis(sh, h)
	if !okx || !oky {
		return 0, 0, false
	}
	return fx, fy, true
}

// downsampleMipBox averages fx×fy whole source texels per destination texel.
// Each tap has area exactly 1, so it accumulates the same float64 terms in the
// same order as downsampleMipArea and encodes with the same functions; the
// output is identical, without per-tap coverage arithmetic. fx and fy are 1 or 2.
func downsampleMipBox(src *image.NRGBA, w, h, fx, fy int, opts MipOptions) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	weight := float64(fx * fy)
	for y := range h {
		out := dst.Pix[y*dst.Stride : y*dst.Stride+w*4]
		for x := range w {
			var sum [4]float64
			var colorWeight float64
			for ty := range fy {
				row := src.Pix[(y*fy+ty)*src.Stride+x*fx*4:]
				for tx := range fx {
					p := row[tx*4 : tx*4+4]
					alpha := unitFloat[p[3]]
					cw := 1.0
					if opts.AlphaWeighted {
						cw = alpha
					}
					if opts.SRGB {
						sum[0] += cw * srgbLinear[p[0]]
						sum[1] += cw * srgbLinear[p[1]]
						sum[2] += cw * srgbLinear[p[2]]
					} else {
						sum[0] += cw * unitFloat[p[0]]
						sum[1] += cw * unitFloat[p[1]]
						sum[2] += cw * unitFloat[p[2]]
					}
					sum[3] += alpha
					colorWeight += cw
				}
			}
			p := out[x*4 : x*4+4]
			if colorWeight > 0 {
				for c := range 3 {
					if opts.SRGB {
						p[c] = encodeSRGB(sum[c] / colorWeight)
					} else {
						p[c] = unitByte(sum[c] / colorWeight)
					}
				}
			}
			p[3] = unitByte(sum[3] / weight)
		}
	}
	return dst
}

// downsampleMipArea is the general area-overlap box filter, unchanged from the
// v0.3.0 implementation. Every source texel contributes the fraction of its area
// under each destination texel, so odd dimensions include their last row and
// column at reduced weight.
func downsampleMipArea(src *image.NRGBA, w, h int, opts MipOptions) *image.NRGBA {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	for y := range h {
		y0, y1 := float64(y)*float64(sh)/float64(h), float64(y+1)*float64(sh)/float64(h)
		for x := range w {
			x0, x1 := float64(x)*float64(sw)/float64(w), float64(x+1)*float64(sw)/float64(w)
			var sum [4]float64
			var weight, colorWeight float64
			for sy := int(y0); sy < min(sh, int(math.Ceil(y1))); sy++ {
				wy := min(y1, float64(sy+1)) - max(y0, float64(sy))
				for sx := int(x0); sx < min(sw, int(math.Ceil(x1))); sx++ {
					area := wy * (min(x1, float64(sx+1)) - max(x0, float64(sx)))
					p := src.Pix[sy*src.Stride+sx*4:][:4]
					alpha := float64(p[3]) / 255
					cw := area
					if opts.AlphaWeighted {
						cw *= alpha
					}
					for c := range 3 {
						value := float64(p[c]) / 255
						if opts.SRGB {
							value = srgbLinear[p[c]]
						}
						sum[c] += cw * value
					}
					sum[3] += area * alpha
					weight += area
					colorWeight += cw
				}
			}
			p := dst.Pix[y*dst.Stride+x*4:][:4]
			if colorWeight > 0 {
				for c := range 3 {
					if opts.SRGB {
						p[c] = encodeSRGB(sum[c] / colorWeight)
					} else {
						p[c] = unitByte(sum[c] / colorWeight)
					}
				}
			}
			p[3] = unitByte(sum[3] / weight)
		}
	}
	return dst
}

// Automatic generation owns these images, so their packed pixels can be passed
// to the upload directly. Avoid a second full-chain copy for large model maps.
func uploadPixels(src image.Image, mipmaps bool, opts MipOptions) ([][]byte, error) {
	if !mipmaps {
		return [][]byte{packPixels(src)}, nil
	}
	levels, err := MipChain(src, opts)
	if err != nil {
		return nil, err
	}
	pixels := make([][]byte, len(levels))
	for i, level := range levels {
		pixels[i] = level.(*image.NRGBA).Pix
	}
	return pixels, nil
}
