package ntgpuimage

import (
	"context"
	"image"
	"image/color"
	"testing"

	"github.com/gogpu/gputypes"
)

func TestMipChain(t *testing.T) {
	for _, srgb := range []bool{false, true} {
		src := image.NewNRGBA(image.Rect(7, 9, 9, 10))
		src.SetNRGBA(7, 9, color.NRGBA{A: 255})
		src.SetNRGBA(8, 9, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
		levels, err := MipChain(src, MipOptions{SRGB: srgb})
		if err != nil {
			t.Fatal(err)
		}
		if len(levels) != 2 {
			t.Fatal(len(levels))
		}
		expected := byte(128)
		if srgb {
			expected = 188
		}
		if got := levels[1].At(0, 0).(color.NRGBA); got != (color.NRGBA{R: expected, G: expected, B: expected, A: 255}) {
			t.Fatalf("srgb=%v: %v", srgb, got)
		}
		levels[0].(*image.NRGBA).Pix[0] = 99
		if src.NRGBAAt(7, 9).R != 0 {
			t.Fatal("source aliased")
		}
	}
	src := image.NewNRGBA(image.Rect(0, 0, 3, 1))
	src.SetNRGBA(2, 0, color.NRGBA{R: 255, A: 255})
	chain, err := MipChain(src, MipOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := chain[1].At(0, 0).(color.NRGBA); got.R != 85 || got.A != 85 {
		t.Fatalf("odd edge dropped: %v", got)
	}
	weighted, err := MipChain(src, MipOptions{AlphaWeighted: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := weighted[1].At(0, 0).(color.NRGBA); got.R != 255 || got.A != 85 {
		t.Fatalf("alpha weighting: %v", got)
	}
	for _, size := range []image.Point{{1, 1}, {384, 256}, {1, 17}, {17, 1}, {7, 5}} {
		levels, err := MipChain(image.NewNRGBA(image.Rectangle{Max: size}), MipOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(levels) != mipLevelCount(size.X, size.Y) || levels[len(levels)-1].Bounds().Size() != (image.Pt(1, 1)) {
			t.Fatalf("bad chain for %v", size)
		}
	}
	if _, err := MipChain(nil, MipOptions{}); err == nil {
		t.Fatal("nil accepted")
	}
}

func TestMipLevelsValidation(t *testing.T) {
	img := func(w, h int) image.Image { return image.NewNRGBA(image.Rect(0, 0, w, h)) }
	for _, levels := range [][]image.Image{nil, {nil}, {img(3, 3), img(2, 2)}, {img(1, 1), img(1, 1)}, {img(4, 4), nil}} {
		if _, _, err := validateLevels("test", levels); err == nil {
			t.Fatalf("accepted %v", levels)
		}
	}
	if _, _, err := validateLevels("test", []image.Image{img(7, 5), img(3, 2), img(1, 1)}); err != nil {
		t.Fatal(err)
	}
	for _, o := range []TextureOptions{{AddressModeU: gputypes.AddressMode(999)}, {AddressModeV: gputypes.AddressMode(999)}} {
		if err := o.validate("test"); err == nil {
			t.Fatal("invalid address accepted")
		}
	}
}

func solidMip(w, h int, c color.NRGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func sampleForTest(t *testing.T, r *Renderer, tex *Texture, expr string) *image.NRGBA {
	t.Helper()
	shader := texBindings + fullScreenVS + "\n@fragment fn fs_main() -> @location(0) vec4<f32> { return " + expr + "; }"
	s, err := newSampler(r, tex, shader)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release(r)
	img, _, err := r.Render(context.Background(), s.draw)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestUploadedMipLevels(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	levels := []image.Image{solidMip(4, 4, color.NRGBA{R: 255, A: 255}), solidMip(2, 2, color.NRGBA{G: 255, A: 128}), solidMip(1, 1, color.NRGBA{B: 255, A: 64})}
	tex, err := r.UploadTextureLevels(levels, TextureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	if tex.MipLevelCount() != 3 {
		t.Fatal(tex.MipLevelCount())
	}
	expectSolid(t, sampleForTest(t, r, tex, "textureSampleLevel(tex,samp,vec2<f32>(0.5),1.0)"), [4]byte{0, 255, 0, 128})
	expectSolid(t, sampleForTest(t, r, tex, "textureSampleLevel(tex,samp,vec2<f32>(0.5),1.5)"), [4]byte{0, 128, 128, 96})
	expectSolid(t, sampleForTest(t, r, tex, "textureSampleGrad(tex,samp,vec2<f32>(0.5),vec2<f32>(0.5,0.0),vec2<f32>(0.0,0.5))"), [4]byte{0, 255, 0, 128})
	if err := tex.Update(levels[0]); err == nil {
		t.Fatal("Update accepted explicit multi-level chain")
	}
	levels[1] = solidMip(2, 2, color.NRGBA{R: 255, G: 255, A: 255})
	if err := tex.UpdateLevels(levels); err != nil {
		t.Fatal(err)
	}
	expectSolid(t, sampleForTest(t, r, tex, "textureLoad(tex,vec2<i32>(0),1)"), [4]byte{255, 255, 0, 255})
	got, err := tex.ReadPixels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, got, levels[0].(*image.NRGBA))
	partial, err := r.UploadTextureLevels(levels[:2], TextureOptions{Filter: gputypes.FilterModeNearest})
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	expectSolid(t, sampleForTest(t, r, partial, "textureSampleLevel(tex,samp,vec2<f32>(0.5),10.0)"), [4]byte{255, 255, 0, 255})
	expectSolid(t, sampleForTest(t, r, partial, "textureSampleLevel(tex,samp,vec2<f32>(0.5),0.75)"), [4]byte{255, 255, 0, 255})

}

func TestAutomaticMipmapsAndAddressModes(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{B: 255, A: 255})
	for _, c := range []struct {
		mode gputypes.AddressMode
		want [4]byte
	}{
		{gputypes.AddressModeClampToEdge, [4]byte{255, 0, 0, 255}},
		{gputypes.AddressModeRepeat, [4]byte{128, 0, 128, 255}},
		{gputypes.AddressModeMirrorRepeat, [4]byte{255, 0, 0, 255}},
	} {
		tex, err := r.UploadTexture(src, TextureOptions{Mipmaps: true, AddressModeU: c.mode, AddressModeV: c.mode})
		if err != nil {
			t.Fatal(err)
		}
		defer tex.Close()
		expectSolid(t, sampleForTest(t, r, tex, "textureSampleLevel(tex,samp,vec2<f32>(0.0,0.5),0.0)"), c.want)
		expectSolid(t, sampleForTest(t, r, tex, "textureSampleLevel(tex,samp,vec2<f32>(-0.5,0.5),1.0)"), [4]byte{128, 0, 128, 255})
		if err := tex.Update(solidMip(2, 1, color.NRGBA{G: 255, A: 255})); err != nil {
			t.Fatal(err)
		}
		expectSolid(t, sampleForTest(t, r, tex, "textureLoad(tex,vec2<i32>(0),1)"), [4]byte{0, 255, 0, 255})
	}
	tex, err := r.UploadTexture(src, TextureOptions{Mipmaps: true, SRGB: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	expectSolid(t, sampleForTest(t, r, tex, "textureLoad(tex,vec2<i32>(0),1)"), [4]byte{128, 0, 128, 255})
}

func TestMinifiedRepeatingChecker(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	src := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			v := byte(0)
			if (x+y)%2 == 0 {
				v = 255
			}
			src.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
	}
	for _, srgb := range []bool{false, true} {
		tex, err := r.UploadTexture(src, TextureOptions{Mipmaps: true, SRGB: srgb, AddressModeU: gputypes.AddressModeRepeat, AddressModeV: gputypes.AddressModeRepeat})
		if err != nil {
			t.Fatal(err)
		}
		// Explicit gradients model eight repeats over the 2x2 output. The tiny
		// footprint must resolve to the chain average, not alternating texels.
		img := sampleForTest(t, r, tex, "textureSampleGrad(tex,samp,vec2<f32>(-2.125,3.375),vec2<f32>(4.0,0.0),vec2<f32>(0.0,4.0))")
		expectSolid(t, img, [4]byte{128, 128, 128, 255})
		if err := tex.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMipUpdatesValidateBeforePacking(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	tex, err := r.NewTexture(4, 4, TextureOptions{Mipmaps: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	if _, err := tex.ReadPixels(context.Background()); err == nil {
		t.Fatal("unwritten mip chain read back")
	}
	// bogusImage panics if pixels are read: reject all bad bounds first.
	bad := &boundsOnlyImage{rect: image.Rect(0, 0, 4, 4)}
	if err := tex.UpdateLevels([]image.Image{bad, solidMip(3, 3, color.NRGBA{}), solidMip(1, 1, color.NRGBA{})}); err == nil {
		t.Fatal("invalid lower level accepted")
	}
	if err := tex.Update(solidMip(4, 4, color.NRGBA{R: 255, A: 255})); err != nil {
		t.Fatal(err)
	}
	expectSolid(t, sampleForTest(t, r, tex, "textureLoad(tex,vec2<i32>(0),2)"), [4]byte{255, 0, 0, 255})
	if err := tex.UpdateLevels([]image.Image{solidMip(4, 4, color.NRGBA{})}); err == nil {
		t.Fatal("wrong level count accepted")
	}
}

type boundsOnlyImage struct{ rect image.Rectangle }

func (b *boundsOnlyImage) Bounds() image.Rectangle { return b.rect }
func (b *boundsOnlyImage) ColorModel() color.Model { return color.NRGBAModel }
func (b *boundsOnlyImage) At(int, int) color.Color {
	panic("pixels read before validating the whole chain")
}

func TestTextureAddressAxes(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	for _, c := range []struct {
		u, v gputypes.AddressMode
		uv   string
		want [4]byte
	}{
		{gputypes.AddressModeRepeat, gputypes.AddressModeClampToEdge, "1.25,1.25", [4]byte{0, 0, 255, 255}},
		{gputypes.AddressModeClampToEdge, gputypes.AddressModeRepeat, "1.25,1.25", [4]byte{0, 255, 0, 255}},
		{gputypes.AddressModeMirrorRepeat, gputypes.AddressModeClampToEdge, "-0.75,0.25", [4]byte{0, 255, 0, 255}},
		{gputypes.AddressModeClampToEdge, gputypes.AddressModeMirrorRepeat, "0.25,-0.75", [4]byte{0, 0, 255, 255}},
	} {
		tex, err := r.UploadTexture(quadrants(), TextureOptions{Filter: gputypes.FilterModeNearest, AddressModeU: c.u, AddressModeV: c.v})
		if err != nil {
			t.Fatal(err)
		}
		expectSolid(t, sampleForTest(t, r, tex, "textureSampleLevel(tex,samp,vec2<f32>("+c.uv+"),0.0)"), c.want)
		if err := tex.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
