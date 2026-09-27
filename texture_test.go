package gpuimage

import (
	"context"
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

func TestImageSizeAndPackPixels(t *testing.T) {
	if _, _, err := imageSize("test", nil); err == nil {
		t.Fatal("nil image accepted")
	}
	if _, _, err := imageSize("test", image.NewNRGBA(image.Rect(0, 0, 0, 2))); err == nil {
		t.Fatal("empty image accepted")
	}
	src := image.NewNRGBA(image.Rect(3, 5, 5, 6))
	src.SetNRGBA(3, 5, color.NRGBA{R: 10, A: 255})
	src.SetNRGBA(4, 5, color.NRGBA{G: 20, A: 255})
	w, h, err := imageSize("test", src)
	if err != nil || w != 2 || h != 1 {
		t.Fatalf("size of a rebased image: %dx%d %v", w, h, err)
	}
	if pix := packPixels(src); len(pix) != 8 || pix[0] != 10 || pix[5] != 20 {
		t.Fatalf("packed pixels: %v", pix)
	}
}

func TestRendererCloseReleasesUploadedTexture(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	src := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	tex, err := r.UploadTexture(src, TextureOptions{Filter: gputypes.FilterModeNearest})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tex.Update(src); !errors.Is(err, ErrClosed) {
		t.Fatalf("Update after renderer close = %v", err)
	}
	if _, err := tex.ReadPixels(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadPixels after renderer close = %v", err)
	}
	if err := tex.Close(); err != nil {
		t.Fatal(err)
	}
}

const sampleTextureHeader = `
@group(0) @binding(0) var tex: texture_2d<f32>;
@group(0) @binding(1) var samp: sampler;
struct Out { @builtin(position) pos: vec4<f32>, @location(0) uv: vec2<f32> };
@fragment fn fs_main(o:Out) -> @location(0) vec4<f32> { return textureSample(tex,samp,o.uv); }
@vertex fn vs_main(@builtin(vertex_index) i: u32) -> Out {
 let p=array<vec2<f32>,6>(vec2<f32>(-1.0,1.0),vec2<f32>(1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,-1.0));
`

// sampleTextureShader draws the texture over the whole target. Its UVs are
// listed explicitly because naga v0.19.0 miscompiles the derived form below
// for Metal; see TestDerivedUVShader.
const sampleTextureShader = sampleTextureHeader + `
 let uv=array<vec2<f32>,6>(vec2<f32>(0.0,0.0),vec2<f32>(1.0,0.0),vec2<f32>(1.0,1.0),vec2<f32>(0.0,0.0),vec2<f32>(1.0,1.0),vec2<f32>(0.0,1.0));
 var o:Out; o.pos=vec4<f32>(p[i],0.0,1.0); o.uv=uv[i]; return o;
}`

// derivedUVShader computes the same UVs from the positions, which is
// arithmetic on components of a bounds-checked array element.
const derivedUVShader = sampleTextureHeader + `
 var o:Out; o.pos=vec4<f32>(p[i],0.0,1.0); o.uv=vec2<f32>(p[i].x*0.5+0.5, 0.5-p[i].y*0.5); return o;
}`

// sampler draws one texture over the whole target with the given shader.
type sampler struct {
	shader   *wgpu.ShaderModule
	layout   *wgpu.PipelineLayout
	pipeline *wgpu.RenderPipeline
	group    *wgpu.BindGroup
}

func newSampler(r *Renderer, tex *Texture, wgsl string) (*sampler, error) {
	s := &sampler{group: tex.BindGroup()}
	err := r.Do(func(dev *wgpu.Device) (err error) {
		s.shader, err = dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{WGSL: wgsl})
		if err != nil {
			return err
		}
		s.layout, err = dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{BindGroupLayouts: []*wgpu.BindGroupLayout{tex.BindGroupLayout()}})
		if err != nil {
			return err
		}
		s.pipeline, err = dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
			Layout: s.layout, Vertex: wgpu.VertexState{Module: s.shader, EntryPoint: "vs_main"},
			Fragment:    &wgpu.FragmentState{Module: s.shader, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{{Format: gputypes.TextureFormatRGBA8Unorm, WriteMask: gputypes.ColorWriteMaskAll}}},
			Primitive:   gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
			Multisample: gputypes.DefaultMultisampleState(),
		})
		return err
	})
	if err != nil {
		s.release(r)
		return nil, err
	}
	return s, nil
}

func (s *sampler) release(r *Renderer) {
	_ = r.Do(func(*wgpu.Device) error {
		if s.pipeline != nil {
			s.pipeline.Release()
		}
		if s.layout != nil {
			s.layout.Release()
		}
		if s.shader != nil {
			s.shader.Release()
		}
		return nil
	})
}

func (s *sampler) draw(f *Frame) error {
	pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{ColorAttachments: []wgpu.RenderPassColorAttachment{{View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore}}})
	if err != nil {
		return err
	}
	pass.SetPipeline(s.pipeline)
	pass.SetBindGroup(0, s.group, nil)
	pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
	return pass.End()
}

// quadrants returns a 2x2 image with a distinct opaque color per pixel.
func quadrants() *image.NRGBA {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{G: 255, A: 255})
	src.SetNRGBA(0, 1, color.NRGBA{B: 255, A: 255})
	src.SetNRGBA(1, 1, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	return src
}

func expectImage(t *testing.T, got, want *image.NRGBA) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("bounds = %v, want %v", got.Bounds(), want.Bounds())
	}
	for y := range want.Bounds().Dy() {
		for x := range want.Bounds().Dx() {
			if got.NRGBAAt(x, y) != want.NRGBAAt(x, y) {
				t.Fatalf("pixel %d,%d = %v, want %v", x, y, got.NRGBAAt(x, y), want.NRGBAAt(x, y))
			}
		}
	}
}

// TestDerivedUVShader tracks an upstream defect. naga v0.19.0 emits the
// bounds check for p[i].x as an unparenthesized conditional, so the
// following arithmetic binds to its fallback operand and Metal rejects the
// generated source. Browsers compile WGSL themselves and are unaffected.
// When this test stops skipping, sampleTextureShader no longer needs
// explicit UVs.
func TestDerivedUVShader(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	src := quadrants()
	tex, err := r.UploadTexture(src, TextureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := newSampler(r, tex, derivedUVShader)
	if err != nil {
		if strings.Contains(err.Error(), "failed to compile MSL") {
			t.Skipf("upstream MSL defect still present: %.160s", err.Error())
		}
		t.Fatal(err)
	}
	defer s.release(r)
	img, _, err := r.Render(context.Background(), s.draw)
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, img, src)
}

func TestSampledTextureUploadUpdateAndReadback(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2, Label: "sampled-test"})
	src := quadrants()
	tex, err := r.UploadTexture(src, TextureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	if w, h := tex.Size(); w != 2 || h != 2 {
		t.Fatalf("size = %dx%d", w, h)
	}
	got, err := tex.ReadPixels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, got, src)
	s, err := newSampler(r, tex, sampleTextureShader)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release(r)
	img, _, err := r.Render(context.Background(), s.draw)
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, img, src)
	if got, err = tex.ReadPixels(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectImage(t, got, src)

	updated := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	for y := range 2 {
		for x := range 2 {
			updated.SetNRGBA(x, y, color.NRGBA{R: 17, G: 33, B: 49, A: 255})
		}
	}
	if err := tex.Update(updated); err != nil {
		t.Fatal(err)
	}
	if got, err = tex.ReadPixels(context.Background()); err != nil {
		t.Fatal(err)
	}
	expectImage(t, got, updated)
	if img, _, err = r.Render(context.Background(), s.draw); err != nil {
		t.Fatal(err)
	}
	expectImage(t, img, updated)
	if err := tex.Update(image.NewNRGBA(image.Rect(0, 0, 1, 1))); err == nil {
		t.Fatal("accepted mismatched update size")
	}
	if err := tex.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tex.Update(updated); err != ErrClosed {
		t.Fatalf("Update after Close = %v", err)
	}
}

// countingImage reports a fixed size and counts pixel reads.
type countingImage struct {
	bounds image.Rectangle
	reads  int
}

func (c *countingImage) ColorModel() color.Model { return color.NRGBAModel }
func (c *countingImage) Bounds() image.Rectangle { return c.bounds }
func (c *countingImage) At(int, int) color.Color {
	c.reads++
	return color.NRGBA{A: 255}
}

// TestUploadRejectsOversizedImageBeforeReadingIt: an image the device cannot
// hold must be rejected from its bounds, without converting its pixels.
func TestUploadRejectsOversizedImageBeforeReadingIt(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	var limit int
	if err := r.Do(func(dev *wgpu.Device) error {
		limit = int(dev.Limits().MaxTextureDimension2D)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if limit <= 0 {
		t.Skip("device reports no texture size limit")
	}
	src := &countingImage{bounds: image.Rect(0, 0, limit+1, 1)}
	if _, err := r.UploadTexture(src, TextureOptions{}); err == nil {
		t.Fatalf("accepted a %d-pixel-wide image above the limit %d", limit+1, limit)
	}
	if src.reads != 0 {
		t.Fatalf("read %d pixels before rejecting the image", src.reads)
	}
}

func TestUpdateRejectsMismatchedImageBeforeReadingIt(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.UploadTexture(quadrants(), TextureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := &countingImage{bounds: image.Rect(0, 0, 64, 64)}
	if err := tex.Update(src); err == nil {
		t.Fatal("accepted mismatched update size")
	}
	if src.reads != 0 {
		t.Fatalf("read %d pixels before rejecting the image", src.reads)
	}
}
