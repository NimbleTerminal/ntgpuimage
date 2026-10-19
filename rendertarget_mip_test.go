package ntgpuimage

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math/rand/v2"
	"testing"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

func TestTextureOptionsMipmappedRenderTargets(t *testing.T) {
	for _, o := range []TextureOptions{
		{Mipmaps: true, RenderTarget: true},
		{Mipmaps: true, RenderTarget: true, SRGB: true, Depth: true},
		{Mipmaps: true, RenderTarget: true, Float16: true},
		{Mipmaps: true, RenderTarget: true, Float16: true, Depth: true, Anisotropy: 16},
	} {
		if err := o.validate("test"); err != nil {
			t.Fatalf("validate(%+v) = %v", o, err)
		}
	}
	if err := (TextureOptions{Mipmaps: true, Float16: true}).validate("test"); err == nil {
		t.Fatal("Float16 without RenderTarget accepted")
	}
}

// patternShader colors each texel from its position: a horizontal and a
// vertical ramp, a 3x5 checker in blue, and alpha alternating with the checker.
// Values are linear light; an SRGB target encodes them on write.
func patternShader(w, h int) string {
	return fmt.Sprintf(`
@vertex fn vs_main(@builtin(vertex_index) i: u32) -> @builtin(position) vec4<f32> {
 let p=array<vec2<f32>,6>(vec2<f32>(-1.0,1.0),vec2<f32>(1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,-1.0));
 return vec4<f32>(p[i],0.5,1.0);
}
@fragment fn fs_main(@builtin(position) p: vec4<f32>) -> @location(0) vec4<f32> {
 let on = ((u32(p.x) / 3u + u32(p.y) / 5u) %% 2u) == 0u;
 let checker = select(0.0, 1.0, on);
 let alpha = select(0.25, 1.0, on);
 return vec4<f32>(p.x / %d.0, p.y / %d.0, checker, alpha);
}`, w, h)
}

// checkerShader draws a one-texel black and white checker.
const checkerShader = `
@vertex fn vs_main(@builtin(vertex_index) i: u32) -> @builtin(position) vec4<f32> {
 let p=array<vec2<f32>,6>(vec2<f32>(-1.0,1.0),vec2<f32>(1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,-1.0));
 return vec4<f32>(p[i],0.5,1.0);
}
@fragment fn fs_main(@builtin(position) p: vec4<f32>) -> @location(0) vec4<f32> {
 let v = select(0.0, 1.0, ((u32(p.x) + u32(p.y)) % 2u) == 0u);
 return vec4<f32>(v, v, v, 1.0);
}`

// drawer draws a full-target quad with a shader into a texture's format,
// attaching the nested frame's depth view when the texture has one.
type drawer struct {
	module   *wgpu.ShaderModule
	layout   *wgpu.PipelineLayout
	pipeline *wgpu.RenderPipeline
}

func newDrawer(t *testing.T, r *Renderer, format gputypes.TextureFormat, depth bool, wgsl string) *drawer {
	t.Helper()
	d := &drawer{}
	err := r.Do(func(dev *wgpu.Device) (err error) {
		if d.module, err = dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{WGSL: wgsl}); err != nil {
			return err
		}
		if d.layout, err = dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{}); err != nil {
			return err
		}
		desc := &wgpu.RenderPipelineDescriptor{
			Layout: d.layout, Vertex: wgpu.VertexState{Module: d.module, EntryPoint: "vs_main"},
			Fragment:    &wgpu.FragmentState{Module: d.module, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{{Format: format, WriteMask: gputypes.ColorWriteMaskAll}}},
			Primitive:   gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
			Multisample: gputypes.DefaultMultisampleState(),
		}
		if depth {
			desc.DepthStencil = &wgpu.DepthStencilState{Format: gputypes.TextureFormatDepth24Plus, DepthWriteEnabled: true, DepthCompare: gputypes.CompareFunctionLess}
		}
		d.pipeline, err = dev.CreateRenderPipeline(desc)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Do(func(*wgpu.Device) error {
			d.pipeline.Release()
			d.layout.Release()
			d.module.Release()
			return nil
		})
	})
	return d
}

func (d *drawer) draw(f *Frame) error {
	desc := &wgpu.RenderPassDescriptor{ColorAttachments: []wgpu.RenderPassColorAttachment{{View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore}}}
	if f.Depth != nil {
		desc.DepthStencilAttachment = &wgpu.RenderPassDepthStencilAttachment{View: f.Depth, DepthLoadOp: gputypes.LoadOpClear, DepthStoreOp: gputypes.StoreOpStore, DepthClearValue: 1, StencilReadOnly: true}
	}
	pass, err := f.Encoder.BeginRenderPass(desc)
	if err != nil {
		return err
	}
	pass.SetPipeline(d.pipeline)
	pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
	return pass.End()
}

// renderInto draws into tex with draw in one frame whose main pass just clears.
func renderInto(t *testing.T, r *Renderer, tex *Texture, draw func(*Frame) error) {
	t.Helper()
	_, _, err := r.Render(context.Background(), func(f *Frame) error {
		if err := f.RenderTo(tex, draw); err != nil {
			return err
		}
		return clearTo(gputypes.Color{})(f)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readLevelForTest(t *testing.T, tex *Texture, level int) *image.NRGBA {
	t.Helper()
	img, err := tex.readLevel(context.Background(), level)
	if err != nil {
		t.Fatalf("level %d: %v", level, err)
	}
	return img
}

// maxChannelDiff returns the largest absolute difference per channel.
func maxChannelDiff(t *testing.T, got, want *image.NRGBA) (diff [4]int) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("bounds = %v, want %v", got.Bounds(), want.Bounds())
	}
	for i := range got.Pix {
		d := int(got.Pix[i]) - int(want.Pix[i])
		if d < 0 {
			d = -d
		}
		diff[i%4] = max(diff[i%4], d)
	}
	return diff
}

// gpuMipTolerance bounds the per-channel difference between a GPU-generated
// level and MipChain of the level-0 readback over a whole chain. The GPU
// filters with its own sRGB decode/encode and float precision, and small
// rounding differences can compound through the chain.
const gpuMipTolerance = 2

func expectChainClose(t *testing.T, name string, tex *Texture, opts MipOptions) {
	t.Helper()
	level0 := readLevelForTest(t, tex, 0)
	want, err := MipChain(level0, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != tex.MipLevelCount() {
		t.Fatalf("%s: MipLevelCount %d, want %d", name, tex.MipLevelCount(), len(want))
	}
	var worst [4]int
	for level := 1; level < len(want); level++ {
		got := readLevelForTest(t, tex, level)
		diff := maxChannelDiff(t, got, want[level].(*image.NRGBA))
		for c := range 4 {
			worst[c] = max(worst[c], diff[c])
			if diff[c] > gpuMipTolerance {
				t.Fatalf("%s: level %d (%v) channel %d differs by %d from MipChain", name, level, got.Bounds().Size(), c, diff[c])
			}
		}
	}
	t.Logf("%s: max per-channel difference from MipChain over %d levels: %v", name, len(want)-1, worst)
}

// TestRenderTargetMipLevelsMatchMipChain draws a pattern into mipmapped
// render targets and compares every generated level with the CPU chain.
func TestRenderTargetMipLevelsMatchMipChain(t *testing.T) {
	for _, c := range []struct {
		name string
		opts TextureOptions
		mip  MipOptions
	}{
		{"plain", TextureOptions{RenderTarget: true, Mipmaps: true}, MipOptions{}},
		{"srgb", TextureOptions{RenderTarget: true, Mipmaps: true, SRGB: true}, MipOptions{SRGB: true}},
		{"srgb+depth", TextureOptions{RenderTarget: true, Mipmaps: true, SRGB: true, Depth: true}, MipOptions{SRGB: true}},
		{"float16+depth", TextureOptions{RenderTarget: true, Mipmaps: true, Float16: true, Depth: true}, MipOptions{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newTestRenderer(t, Options{Width: 2, Height: 2})
			const w, h = 64, 32
			tex, err := r.NewTexture(w, h, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer tex.Close()
			if tex.MipLevelCount() != mipLevelCount(w, h) {
				t.Fatalf("MipLevelCount = %d", tex.MipLevelCount())
			}
			d := newDrawer(t, r, tex.Format(), c.opts.Depth, patternShader(w, h))
			var seen Frame
			renderInto(t, r, tex, func(f *Frame) error {
				seen = *f
				return d.draw(f)
			})
			if (seen.Depth != nil) != c.opts.Depth || seen.Width != w || seen.Height != h {
				t.Fatalf("nested frame %+v", seen)
			}
			expectChainClose(t, c.name, tex, c.mip)
			// ReadPixels still returns level 0.
			got, err := tex.ReadPixels(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			expectImage(t, got, readLevelForTest(t, tex, 0))
			// A second frame regenerates from the new contents. The clear color
			// is linear; an SRGB target stores it encoded.
			renderInto(t, r, tex, clearTo(gputypes.Color{R: 0.2, G: 0.6, B: 0.4, A: 1}))
			want := [4]byte{51, 153, 102, 255}
			if c.opts.SRGB {
				want = [4]byte{encodeSRGB(0.2), encodeSRGB(0.6), encodeSRGB(0.4), 255}
			}
			for level := range tex.MipLevelCount() {
				expectSolid(t, readLevelForTest(t, tex, level), want)
			}
		})
	}
}

// TestRenderTargetMipOddSizes: a 97x61 target has the full chain down to 1x1
// and a solid fill stays exact at every level.
func TestRenderTargetMipOddSizes(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	tex, err := r.NewTexture(97, 61, TextureOptions{RenderTarget: true, Mipmaps: true, SRGB: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	if tex.MipLevelCount() != 7 {
		t.Fatalf("MipLevelCount = %d, want 7", tex.MipLevelCount())
	}
	renderInto(t, r, tex, clearTo(gputypes.Color{R: 1, G: 0.5, B: 0.25, A: 0.5}))
	w, h := 97, 61
	for level := range tex.MipLevelCount() {
		img := readLevelForTest(t, tex, level)
		if img.Bounds().Dx() != w || img.Bounds().Dy() != h {
			t.Fatalf("level %d is %v, want %dx%d", level, img.Bounds().Size(), w, h)
		}
		// Linear 1, 0.5, 0.25 encode to 255, 188, 137; alpha stays linear.
		expectSolid(t, img, [4]byte{255, 188, 137, 128})
		w, h = max(1, w/2), max(1, h/2)
	}
	if w != 1 || h != 1 {
		t.Fatal("chain did not end at 1x1")
	}
}

// TestRenderTargetMipSampling: a minifying sample over a one-texel checker
// reaches the generated levels and resolves to the chain average.
func TestRenderTargetMipSampling(t *testing.T) {
	for _, srgb := range []bool{false, true} {
		t.Run(fmt.Sprintf("srgb=%v", srgb), func(t *testing.T) {
			r := newTestRenderer(t, Options{Width: 2, Height: 2})
			tex, err := r.NewTexture(64, 64, TextureOptions{RenderTarget: true, Mipmaps: true, SRGB: srgb, AddressModeU: gputypes.AddressModeRepeat, AddressModeV: gputypes.AddressModeRepeat})
			if err != nil {
				t.Fatal(err)
			}
			defer tex.Close()
			d := newDrawer(t, r, tex.Format(), false, checkerShader)
			renderInto(t, r, tex, d.draw)
			img := sampleForTest(t, r, tex, "textureSampleGrad(tex,samp,vec2<f32>(-2.125,3.375),vec2<f32>(4.0,0.0),vec2<f32>(0.0,4.0))")
			expectSolid(t, img, [4]byte{128, 128, 128, 255})
			// Level 0 itself is still the checker.
			expectSolid(t, sampleForTest(t, r, tex, "textureLoad(tex,vec2<i32>(0),0)"), [4]byte{255, 255, 255, 255})
			expectSolid(t, sampleForTest(t, r, tex, "textureLoad(tex,vec2<i32>(1,0),0)"), [4]byte{0, 0, 0, 255})
		})
	}
}

// TestRenderTargetMipUpdates: Update regenerates the chain on the GPU,
// UpdateLevels uploads the supplied levels, and the next RenderTo replaces
// them all.
func TestRenderTargetMipUpdates(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	tex, err := r.NewTexture(8, 8, TextureOptions{RenderTarget: true, Mipmaps: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	renderInto(t, r, tex, clearTo(gputypes.Color{R: 1, A: 1}))
	if err := tex.Update(solid(8, 8, color.NRGBA{G: 255, A: 255})); err != nil {
		t.Fatal(err)
	}
	for level := range 4 {
		expectSolid(t, readLevelForTest(t, tex, level), [4]byte{0, 255, 0, 255})
	}
	levels := []image.Image{solid(8, 8, color.NRGBA{G: 255, A: 255}), solid(4, 4, color.NRGBA{B: 255, A: 255}), solid(2, 2, color.NRGBA{B: 255, A: 255}), solid(1, 1, color.NRGBA{B: 255, A: 255})}
	if err := tex.UpdateLevels(levels); err != nil {
		t.Fatal(err)
	}
	expectSolid(t, readLevelForTest(t, tex, 0), [4]byte{0, 255, 0, 255})
	expectSolid(t, readLevelForTest(t, tex, 1), [4]byte{0, 0, 255, 255})
	renderInto(t, r, tex, clearTo(gputypes.Color{R: 1, G: 1, A: 1}))
	for level := range 4 {
		expectSolid(t, readLevelForTest(t, tex, level), [4]byte{255, 255, 0, 255})
	}
	// Supplied chains can be render targets too; the next pass regenerates.
	supplied, err := r.UploadTextureLevels(levels, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer supplied.Close()
	expectSolid(t, readLevelForTest(t, supplied, 1), [4]byte{0, 0, 255, 255})
	renderInto(t, r, supplied, clearTo(gputypes.Color{B: 1, G: 1, A: 1}))
	expectSolid(t, readLevelForTest(t, supplied, 1), [4]byte{0, 255, 255, 255})
	if _, err := tex.readLevel(context.Background(), 4); err == nil {
		t.Fatal("level beyond the chain read")
	}
}

// TestRenderTargetMipOncePerFrame: a second RenderTo into the same mipmapped
// target in one frame is rejected before its callback runs, the frame stays
// usable, and the first drawing fills every level.
func TestRenderTargetMipOncePerFrame(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	tex, err := r.NewTexture(16, 16, TextureOptions{RenderTarget: true, Mipmaps: true, SRGB: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		if err := f.RenderTo(tex, clearTo(gputypes.Color{B: 1, A: 1})); err != nil {
			return err
		}
		ran := false
		if err := f.RenderTo(tex, func(*Frame) error { ran = true; return nil }); err == nil || ran {
			return errors.New("second RenderTo in one frame accepted")
		}
		return clearTo(gputypes.Color{})(f)
	})
	if err != nil {
		t.Fatal(err)
	}
	for level := range tex.MipLevelCount() {
		expectSolid(t, readLevelForTest(t, tex, level), [4]byte{0, 0, 255, 255})
	}
	// The next frame draws again, and a later frame samples the chain.
	renderInto(t, r, tex, clearTo(gputypes.Color{R: 1, A: 1}))
	expectSolid(t, sampleForTest(t, r, tex, "textureSampleLevel(tex,samp,vec2<f32>(0.5),3.0)"), [4]byte{255, 0, 0, 255})
}

func TestRenderTargetMipFailedPassLeavesRendererUsable(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.NewTexture(4, 4, TextureOptions{RenderTarget: true, Mipmaps: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	boom := errors.New("boom")
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		return f.RenderTo(tex, func(*Frame) error { return boom })
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := tex.ReadPixels(context.Background()); err == nil {
		t.Fatal("unwritten texture read")
	}
	renderInto(t, r, tex, clearTo(gputypes.Color{A: 1}))
	expectSolid(t, readLevelForTest(t, tex, 2), [4]byte{0, 0, 0, 255})
}

// TestRenderTargetMipUpdateFirst makes Update the first write to a blank
// mipmapped render target. The lower levels have never been written, so the
// regeneration pass must start them from the layout WriteTexture left them
// in rather than one a RenderTo would have set. The CPU chain of the
// uploaded image is the reference.
func TestRenderTargetMipUpdateFirst(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	for _, tc := range []struct {
		name string
		opts TextureOptions
		mips MipOptions
	}{
		{"plain", TextureOptions{RenderTarget: true, Mipmaps: true}, MipOptions{}},
		{"srgb", TextureOptions{RenderTarget: true, Mipmaps: true, SRGB: true}, MipOptions{SRGB: true}},
		{"depth", TextureOptions{RenderTarget: true, Mipmaps: true, Depth: true}, MipOptions{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tex, err := r.NewTexture(64, 32, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer tex.Close()
			src := adversarialNRGBA(rand.New(rand.NewPCG(7, 11)), image.Rect(0, 0, 64, 32))
			if err := tex.Update(src); err != nil {
				t.Fatal(err)
			}
			expectChainClose(t, tc.name+" first update", tex, tc.mips)
			// A second Update and a RenderTo must chain onto the layouts the
			// first regeneration left behind.
			if err := tex.Update(solid(64, 32, color.NRGBA{R: 255, A: 255})); err != nil {
				t.Fatal(err)
			}
			expectSolid(t, readLevelForTest(t, tex, tex.MipLevelCount()-1), [4]byte{255, 0, 0, 255})
			renderInto(t, r, tex, clearTo(gputypes.Color{G: 1, A: 1}))
			expectSolid(t, readLevelForTest(t, tex, tex.MipLevelCount()-1), [4]byte{0, 255, 0, 255})
		})
	}
}
