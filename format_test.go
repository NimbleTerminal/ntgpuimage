package ntgpuimage

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

func TestTextureOptionsFormats(t *testing.T) {
	for _, o := range []TextureOptions{
		{Float16: true},
		{Float16: true, SRGB: true, RenderTarget: true},
	} {
		if err := o.validate("test"); err == nil {
			t.Fatalf("validate(%+v) accepted", o)
		}
	}
	for _, c := range []struct {
		opts TextureOptions
		want gputypes.TextureFormat
	}{
		{TextureOptions{}, gputypes.TextureFormatRGBA8Unorm},
		{TextureOptions{RenderTarget: true, Depth: true}, gputypes.TextureFormatRGBA8Unorm},
		{TextureOptions{SRGB: true}, gputypes.TextureFormatRGBA8UnormSrgb},
		{TextureOptions{SRGB: true, RenderTarget: true, Depth: true}, gputypes.TextureFormatRGBA8UnormSrgb},
		{TextureOptions{Float16: true, RenderTarget: true}, gputypes.TextureFormatRGBA16Float},
		{TextureOptions{Float16: true, RenderTarget: true, Depth: true}, gputypes.TextureFormatRGBA16Float},
	} {
		if err := c.opts.validate("test"); err != nil {
			t.Fatalf("validate(%+v) = %v", c.opts, err)
		}
		if got := c.opts.format(); got != c.want {
			t.Fatalf("format(%+v) = %v, want %v", c.opts, got, c.want)
		}
	}
}

// fullScreenVS draws one full-target quad with no UVs.
const fullScreenVS = `
@vertex fn vs_main(@builtin(vertex_index) i: u32) -> @builtin(position) vec4<f32> {
 let p=array<vec2<f32>,6>(vec2<f32>(-1.0,1.0),vec2<f32>(1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,-1.0));
 return vec4<f32>(p[i],0.0,1.0);
}
fn encode(c: f32) -> f32 {
 if (c <= 0.0031308) { return c * 12.92; }
 return 1.055 * pow(c, 1.0 / 2.4) - 0.055;
}
`

const texBindings = `
@group(0) @binding(0) var tex: texture_2d<f32>;
@group(0) @binding(1) var samp: sampler;
`

// midSampleShader samples exactly halfway between the two texels of a 2x1
// texture and writes the sample unchanged.
const midSampleShader = texBindings + fullScreenVS + `
@fragment fn fs_main() -> @location(0) vec4<f32> {
 let s = textureSample(tex, samp, vec2<f32>(0.5, 0.5));
 return vec4<f32>(s.rgb, 1.0);
}`

// midSampleEncodedShader writes the same sample sRGB-encoded.
const midSampleEncodedShader = texBindings + fullScreenVS + `
@fragment fn fs_main() -> @location(0) vec4<f32> {
 let s = textureSample(tex, samp, vec2<f32>(0.5, 0.5));
 return vec4<f32>(encode(s.r), encode(s.g), encode(s.b), 1.0);
}`

// TestSRGBTextureFiltersInLinearLight samples a black and a white texel
// halfway between them. An sRGB texture decodes before filtering, so the
// shader sees 0.5 linear, whose encoding is 188. A plain texture filters
// the encoded bytes and the shader sees encoded 0.5 (128), which is only
// about 21% light.
func TestSRGBTextureFiltersInLinearLight(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	src := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	src.SetNRGBA(0, 0, color.NRGBA{A: 255})
	src.SetNRGBA(1, 0, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	for _, c := range []struct {
		name   string
		srgb   bool
		shader string
		want   byte
	}{
		// Written unchanged, 0.5 linear and 0.5 encoded both read back as 128.
		{"srgb", true, midSampleShader, 128},
		{"plain", false, midSampleShader, 128},
		// The shader turns the filtered light into an encoded value: an sRGB
		// sample is linear and needs encoding, a plain sample already is.
		{"srgb encoded", true, midSampleEncodedShader, 188},
		{"plain as encoded", false, midSampleShader, 128},
	} {
		t.Run(c.name, func(t *testing.T) {
			tex, err := r.UploadTexture(src, TextureOptions{SRGB: c.srgb})
			if err != nil {
				t.Fatal(err)
			}
			defer tex.Close()
			s, err := newSampler(r, tex, c.shader)
			if err != nil {
				t.Fatal(err)
			}
			defer s.release(r)
			img, _, err := r.Render(context.Background(), s.draw)
			if err != nil {
				t.Fatal(err)
			}
			expectSolid(t, img, [4]byte{c.want, c.want, c.want, 255})
		})
	}
}

// allBytes returns a 16x16 opaque image whose colour channels each hold
// all 256 byte values, in different orders.
func allBytes() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for i := range 256 {
		img.SetNRGBA(i%16, i/16, color.NRGBA{R: byte(i), G: byte(255 - i), B: byte(i) ^ 0x5a, A: 255})
	}
	return img
}

// TestSRGBTextureRoundTrip: sRGB storage changes only what shaders see. On
// both readback paths ReadPixels returns the stored bytes exactly.
//
// Each case has its own renderer. gogpu v0.34.5 frees a texture's tracker
// index on Release without clearing the device tracker's state for it, so
// a texture that reuses the index inherits a stale usage, and on Vulkan
// its first submission starts with a barrier from the wrong layout.
func TestSRGBTextureRoundTrip(t *testing.T) {
	want := allBytes()
	check := func(t *testing.T, tex *Texture) {
		t.Helper()
		if f := tex.Format(); f != gputypes.TextureFormatRGBA8UnormSrgb {
			t.Fatalf("format = %v", f)
		}
		got, err := tex.ReadPixels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		expectImage(t, got, want)
	}
	t.Run("upload", func(t *testing.T) {
		r := newTestRenderer(t, Options{Width: 1, Height: 1})
		tex, err := r.UploadTexture(want, TextureOptions{SRGB: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tex.Close()
		check(t, tex)
	})
	t.Run("update", func(t *testing.T) {
		r := newTestRenderer(t, Options{Width: 1, Height: 1})
		tex, err := r.NewTexture(16, 16, TextureOptions{SRGB: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tex.Close()
		if err := tex.Update(want); err != nil {
			t.Fatal(err)
		}
		check(t, tex)
	})
	// A pass that loads the texture marks it drawn, so ReadPixels takes the
	// blit path, which samples and therefore decodes.
	for _, depth := range []bool{false, true} {
		name := "drawn"
		if depth {
			name += " with depth"
		}
		t.Run(name, func(t *testing.T) {
			r := newTestRenderer(t, Options{Width: 1, Height: 1})
			tex, err := r.NewTexture(16, 16, TextureOptions{SRGB: true, RenderTarget: true, Depth: depth})
			if err != nil {
				t.Fatal(err)
			}
			defer tex.Close()
			if err := tex.Update(want); err != nil {
				t.Fatal(err)
			}
			if _, err := r.RenderToTexture(context.Background(), func(f *Frame) error {
				return f.RenderTo(tex, loadOnly)
			}); err != nil {
				t.Fatal(err)
			}
			check(t, tex)
		})
	}
}

// constShader writes one colour; blendShader writes white at half alpha.
const constShader = fullScreenVS + `
@fragment fn fs_main() -> @location(0) vec4<f32> { return vec4<f32>(0.5, 0.5, 0.5, 1.0); }`

const blendShader = fullScreenVS + `
@fragment fn fs_main() -> @location(0) vec4<f32> { return vec4<f32>(1.0, 1.0, 1.0, 0.5); }`

// pipeline is a full-target draw with no bindings.
type pipeline struct {
	module *wgpu.ShaderModule
	layout *wgpu.PipelineLayout
	p      *wgpu.RenderPipeline
}

func newPipeline(t *testing.T, r *Renderer, wgsl string, target gputypes.ColorTargetState) *pipeline {
	t.Helper()
	pl := &pipeline{}
	err := r.Do(func(dev *wgpu.Device) (err error) {
		if pl.module, err = dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{WGSL: wgsl}); err != nil {
			return err
		}
		if pl.layout, err = dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{}); err != nil {
			return err
		}
		pl.p, err = dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
			Layout: pl.layout, Vertex: wgpu.VertexState{Module: pl.module, EntryPoint: "vs_main"},
			Fragment:    &wgpu.FragmentState{Module: pl.module, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{target}},
			Primitive:   gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
			Multisample: gputypes.DefaultMultisampleState(),
		})
		return err
	})
	t.Cleanup(func() {
		_ = r.Do(func(*wgpu.Device) error {
			if pl.p != nil {
				pl.p.Release()
			}
			if pl.layout != nil {
				pl.layout.Release()
			}
			if pl.module != nil {
				pl.module.Release()
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// drawOver clears f's target to clear and draws the pipeline over it.
func (pl *pipeline) drawOver(clear gputypes.Color) func(*Frame) error {
	return func(f *Frame) error {
		pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{ColorAttachments: []wgpu.RenderPassColorAttachment{{
			View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore, ClearValue: clear,
		}}})
		if err != nil {
			return err
		}
		pass.SetPipeline(pl.p)
		pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
		return pass.End()
	}
}

// TestRenderIntoSRGBTarget: an sRGB render target encodes what the shader
// writes and blends in linear light, while a plain one stores and blends
// the values as given.
func TestRenderIntoSRGBTarget(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	black := gputypes.Color{A: 1}
	alphaBlend := &gputypes.BlendState{
		Color: gputypes.BlendComponent{SrcFactor: gputypes.BlendFactorSrcAlpha, DstFactor: gputypes.BlendFactorOneMinusSrcAlpha, Operation: gputypes.BlendOperationAdd},
		Alpha: gputypes.BlendComponent{SrcFactor: gputypes.BlendFactorOne, DstFactor: gputypes.BlendFactorOneMinusSrcAlpha, Operation: gputypes.BlendOperationAdd},
	}
	for _, c := range []struct {
		name   string
		srgb   bool
		shader string
		blend  *gputypes.BlendState
		want   byte
	}{
		{"srgb linear 0.5", true, constShader, nil, 188},
		{"plain 0.5", false, constShader, nil, 128},
		{"srgb blend", true, blendShader, alphaBlend, 188},
		{"plain blend", false, blendShader, alphaBlend, 128},
	} {
		t.Run(c.name, func(t *testing.T) {
			tex, err := r.NewTexture(2, 2, TextureOptions{SRGB: c.srgb, RenderTarget: true, Depth: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tex.Close()
			pl := newPipeline(t, r, c.shader, gputypes.ColorTargetState{Format: tex.Format(), Blend: c.blend, WriteMask: gputypes.ColorWriteMaskAll})
			sample, err := newSampler(r, tex, sampleTextureShader)
			if err != nil {
				t.Fatal(err)
			}
			defer sample.release(r)
			// Sampling the result in the same frame decodes it again: the
			// main target receives the linear value the shader wrote.
			img, _, err := r.Render(context.Background(), func(f *Frame) error {
				if err := f.RenderTo(tex, pl.drawOver(black)); err != nil {
					return err
				}
				return sample.draw(f)
			})
			if err != nil {
				t.Fatal(err)
			}
			expectSolid(t, img, [4]byte{128, 128, 128, 255})
			got, err := tex.ReadPixels(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			expectSolid(t, got, [4]byte{c.want, c.want, c.want, 255})
		})
	}
}

// loadShader reads texel (0,0) with textureLoad and scales it by a quarter,
// so a Float16 value of 2.0 arrives as 0.5.
const loadShader = texBindings + fullScreenVS + `
@fragment fn fs_main() -> @location(0) vec4<f32> { return textureLoad(tex, vec2<i32>(0, 0), 0) * 0.25; }`

func TestFloat16RenderTarget(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	if _, err := r.NewTexture(2, 2, TextureOptions{Float16: true}); err == nil {
		t.Fatal("Float16 without RenderTarget accepted")
	}
	if _, err := r.UploadTexture(quadrants(), TextureOptions{Float16: true, RenderTarget: true}); err == nil {
		t.Fatal("UploadTexture accepted Float16")
	}
	tex, err := r.NewTexture(2, 2, TextureOptions{Float16: true, RenderTarget: true, Depth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	if f := tex.Format(); f != gputypes.TextureFormatRGBA16Float {
		t.Fatalf("format = %v", f)
	}
	if err := tex.Update(quadrants()); err == nil {
		t.Fatal("Update accepted a Float16 texture")
	}
	if _, err := tex.ReadPixels(context.Background()); err == nil {
		t.Fatal("ReadPixels succeeded before anything was drawn")
	}
	load, err := newSampler(r, tex, loadShader)
	if err != nil {
		t.Fatal(err)
	}
	defer load.release(r)
	img, _, err := r.Render(context.Background(), func(f *Frame) error {
		if err := f.RenderTo(tex, func(inner *Frame) error {
			if inner.Depth == nil {
				return errors.New("nested frame has no depth view")
			}
			return clearTo(gputypes.Color{R: 2, G: 0.5, B: 0.25, A: 1})(inner)
		}); err != nil {
			return err
		}
		return load.draw(f)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Values above 1 survive in the texture.
	expectSolid(t, img, [4]byte{128, 32, 16, 64})
	// ReadPixels clamps to 8 bits.
	got, err := tex.ReadPixels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expectSolid(t, got, [4]byte{255, 128, 64, 255})
}
