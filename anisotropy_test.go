package ntgpuimage

import (
	"context"
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/gogpu/gputypes"
)

func TestTextureOptionsAnisotropy(t *testing.T) {
	for _, filter := range []gputypes.FilterMode{0, gputypes.FilterModeLinear, gputypes.FilterModeNearest} {
		for _, n := range []uint16{0, 1, 2, 3, 8, 16, 17, 65535} {
			opts := TextureOptions{Filter: filter, Anisotropy: n}
			err := opts.validate("test")
			valid := n <= 16 && (n <= 1 || filter != gputypes.FilterModeNearest)
			if (err == nil) != valid {
				t.Fatalf("validate(%+v) = %v, valid=%v", opts, err, valid)
			}
		}
	}
	// Sampler-only: no requirement for mipmaps or a particular storage format.
	for _, opts := range []TextureOptions{
		{Anisotropy: 16}, {Anisotropy: 16, Mipmaps: true},
		{Anisotropy: 16, SRGB: true}, {Anisotropy: 16, RenderTarget: true},
		{Anisotropy: 16, RenderTarget: true, Float16: true, Depth: true},
	} {
		if err := opts.validate("test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnisotropyTexturePaths(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	// Each public constructor must reject bad sampler options before reading pixels.
	bad := TextureOptions{Filter: gputypes.FilterModeNearest, Anisotropy: 2}
	if _, err := r.UploadTexture(&boundsOnlyImage{rect: image.Rect(0, 0, 2, 2)}, bad); err == nil {
		t.Fatal("upload accepted nearest anisotropy")
	}
	if _, err := r.UploadTextureLevels([]image.Image{&boundsOnlyImage{rect: image.Rect(0, 0, 2, 2)}}, bad); err == nil {
		t.Fatal("levels accepted nearest anisotropy")
	}
	if _, err := r.NewTexture(2, 2, bad); err == nil {
		t.Fatal("new texture accepted nearest anisotropy")
	}
	if _, err := r.NewTexture(2, 2, TextureOptions{Anisotropy: 17}); err == nil {
		t.Fatal("anisotropy above 16 accepted")
	}
	for _, n := range []uint16{0, 1, 2, 16} {
		tex, err := r.UploadTexture(solidMip(2, 2, color.NRGBA{R: 255, A: 255}), TextureOptions{Anisotropy: n})
		if err != nil {
			t.Fatal(err)
		}
		expectSolid(t, sampleForTest(t, r, tex, "textureSample(tex,samp,vec2<f32>(0.5))"), [4]byte{255, 0, 0, 255})
		defer tex.Close()
		tex, err = r.UploadTextureLevels([]image.Image{solidMip(2, 2, color.NRGBA{G: 255, A: 255}), solidMip(1, 1, color.NRGBA{G: 255, A: 255})}, TextureOptions{Anisotropy: n})
		if err != nil {
			t.Fatal(err)
		}
		expectSolid(t, sampleForTest(t, r, tex, "textureSampleLevel(tex,samp,vec2<f32>(0.5),1.0)"), [4]byte{0, 255, 0, 255})
		defer tex.Close()
		for _, opts := range []TextureOptions{{RenderTarget: true, Anisotropy: n}, {RenderTarget: true, SRGB: true, Anisotropy: n}, {RenderTarget: true, Float16: true, Anisotropy: n}} {
			target, err := r.NewTexture(2, 2, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			if _, err := r.RenderToTexture(context.Background(), func(f *Frame) error { return f.RenderTo(target, clearTo(gputypes.Color{B: 1, A: 1})) }); err != nil {
				t.Fatal(err)
			}
			expectSolid(t, sampleForTest(t, r, target, "textureSample(tex,samp,vec2<f32>(0.5))"), [4]byte{0, 0, 255, 255})
		}
	}
}

// A perspective-projected horizontal plane. Near the horizon (the top), the
// footprint stretches much farther along v than u. The repeating checker should
// retain more contrast with anisotropic sampling than with isotropic mip LOD.
const grazingPlaneShader = texBindings + fullScreenVS + `
@fragment fn fs_main(@builtin(position) p: vec4<f32>) -> @location(0) vec4<f32> {
 let screen=p.xy/vec2<f32>(256.0);
 let uv=vec2<f32>((screen.x-0.5)*0.1,0.5)/(screen.y+0.05);
 return textureSample(tex,samp,uv);
}`

func TestAnisotropyGrazingChecker(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 256, Height: 256})
	src := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	for y := range 512 {
		for x := range 512 {
			v := byte(0)
			if (x/16+y/16)%2 == 0 {
				v = 255
			}
			src.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
	}
	levels, err := MipChain(src, MipOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var contrasts [2]float64
	for i, n := range []uint16{1, 16} {
		tex, err := r.UploadTextureLevels(levels, TextureOptions{Anisotropy: n, AddressModeU: gputypes.AddressModeRepeat, AddressModeV: gputypes.AddressModeRepeat})
		if err != nil {
			t.Fatal(err)
		}
		defer tex.Close()
		s, err := newSampler(r, tex, grazingPlaneShader)
		if err != nil {
			t.Fatal(err)
		}
		defer s.release(r)
		img, _, err := r.Render(context.Background(), s.draw)
		if err != nil {
			t.Fatal(err)
		}
		// Measure RMS contrast around midgray in a far band, avoiding the horizon
		// where even the elongated footprint spans many complete checker squares.
		var sum float64
		count := 0
		for y := 38; y < 90; y++ {
			for x := 16; x < 240; x++ {
				d := float64(img.NRGBAAt(x, y).R) - 127.5
				sum += d * d
				count++
			}
		}
		contrasts[i] = math.Sqrt(sum / float64(count))
	}
	t.Logf("far checker RMS contrast: off=%.2f 16x=%.2f", contrasts[0], contrasts[1])
	if contrasts[1] < contrasts[0]*1.1+2 {
		t.Fatalf("anisotropy did not improve far checker contrast: %v", contrasts)
	}
}
