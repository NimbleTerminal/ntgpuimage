package ntgpuimage

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"testing"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

func TestTextureOptionsRejectDepthWithoutRenderTarget(t *testing.T) {
	if err := (TextureOptions{Depth: true}).validate("test"); err == nil {
		t.Fatal("Depth without RenderTarget accepted")
	}
	for _, o := range []TextureOptions{{}, {RenderTarget: true}, {RenderTarget: true, Depth: true}} {
		if err := o.validate("test"); err != nil {
			t.Fatalf("validate(%+v) = %v", o, err)
		}
	}
}

func TestNewTextureRejectsInvalidSize(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	for _, s := range [][2]int{{0, 1}, {1, 0}, {-1, 1}} {
		if _, err := r.NewTexture(s[0], s[1], TextureOptions{}); err == nil {
			t.Fatalf("NewTexture(%d, %d) accepted", s[0], s[1])
		}
	}
	if _, err := r.NewTexture(1, 1, TextureOptions{Depth: true}); err == nil {
		t.Fatal("NewTexture with Depth but no RenderTarget accepted")
	}
}

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// TestRenderToTextureThenSample draws into a texture and samples it into the
// main target within one frame, then repeats with another colour so the
// second frame exercises the barrier before the pass as well as after it.
func TestRenderToTextureThenSample(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	tex, err := r.NewTexture(2, 2, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	if w, h := tex.Size(); w != 2 || h != 2 {
		t.Fatalf("size = %dx%d", w, h)
	}
	s, err := newSampler(r, tex, sampleTextureShader)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release(r)

	colours := []struct {
		clear gputypes.Color
		want  color.NRGBA
	}{
		{gputypes.Color{R: 1, A: 1}, color.NRGBA{R: 255, A: 255}},
		{gputypes.Color{G: 1, A: 1}, color.NRGBA{G: 255, A: 255}},
		{gputypes.Color{B: 1, A: 1}, color.NRGBA{B: 255, A: 255}},
	}
	for i, c := range colours {
		var sub *Frame
		var seen Frame
		img, _, err := r.Render(context.Background(), func(f *Frame) error {
			if err := f.RenderTo(tex, func(inner *Frame) error {
				sub, seen = inner, *inner
				if inner.Encoder != f.Encoder || inner.Device != f.Device {
					return errors.New("nested frame does not share the encoder and device")
				}
				return clearTo(c.clear)(inner)
			}); err != nil {
				return err
			}
			return s.draw(f)
		})
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		expectImage(t, img, solid(2, 2, c.want))
		got, err := tex.ReadPixels(context.Background())
		if err != nil {
			t.Fatalf("frame %d: ReadPixels: %v", i, err)
		}
		expectImage(t, got, solid(2, 2, c.want))
		if seen.Width != 2 || seen.Height != 2 || seen.Color == nil || seen.Depth != nil {
			t.Fatalf("frame %d: nested frame = %+v", i, seen)
		}
		if seen.Seq != uint64(i+1) {
			t.Fatalf("frame %d: nested Seq = %d", i, seen.Seq)
		}
		// The nested Frame is valid only inside its callback.
		if sub.Encoder != nil || sub.Color != nil {
			t.Fatalf("frame %d: nested frame still usable after RenderTo: %+v", i, sub)
		}
	}
}

// TestRenderToUploadedTexture draws over an uploaded image, which rests in
// the sampled layout before the first pass.
func TestRenderToUploadedTexture(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	tex, err := r.UploadTexture(quadrants(), TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	s, err := newSampler(r, tex, sampleTextureShader)
	if err != nil {
		t.Fatal(err)
	}
	defer s.release(r)
	img, _, err := r.Render(context.Background(), s.draw)
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, img, quadrants())
	img, _, err = r.Render(context.Background(), func(f *Frame) error {
		if err := f.RenderTo(tex, clearTo(gputypes.Color{R: 1, G: 1, A: 1})); err != nil {
			return err
		}
		return s.draw(f)
	})
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, img, solid(2, 2, color.NRGBA{R: 255, G: 255, A: 255}))
}

// depthShader draws two full-target quads in one call: instance 0 is red at
// depth 0.25, instance 1 is green at depth 0.75. With a Less depth test the
// red quad wins regardless of draw order; without one, green would.
const depthShader = `
struct Out { @builtin(position) pos: vec4<f32>, @location(0) col: vec4<f32> };
@vertex fn vs_main(@builtin(vertex_index) i: u32, @builtin(instance_index) n: u32) -> Out {
 let p=array<vec2<f32>,6>(vec2<f32>(-1.0,1.0),vec2<f32>(1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,1.0),vec2<f32>(1.0,-1.0),vec2<f32>(-1.0,-1.0));
 var o:Out;
 if (n == 0u) { o.pos=vec4<f32>(p[i],0.25,1.0); o.col=vec4<f32>(1.0,0.0,0.0,1.0); }
 else { o.pos=vec4<f32>(p[i],0.75,1.0); o.col=vec4<f32>(0.0,1.0,0.0,1.0); }
 return o;
}
@fragment fn fs_main(o:Out) -> @location(0) vec4<f32> { return o.col; }`

func TestRenderToWithDepthTestsDepth(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2, Height: 2})
	tex, err := r.NewTexture(2, 2, TextureOptions{RenderTarget: true, Depth: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	var module *wgpu.ShaderModule
	var layout *wgpu.PipelineLayout
	var pipeline *wgpu.RenderPipeline
	err = r.Do(func(dev *wgpu.Device) (err error) {
		if module, err = dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{WGSL: depthShader}); err != nil {
			return err
		}
		if layout, err = dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{}); err != nil {
			return err
		}
		pipeline, err = dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
			Layout: layout, Vertex: wgpu.VertexState{Module: module, EntryPoint: "vs_main"},
			Fragment:     &wgpu.FragmentState{Module: module, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{{Format: gputypes.TextureFormatRGBA8Unorm, WriteMask: gputypes.ColorWriteMaskAll}}},
			Primitive:    gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
			DepthStencil: &wgpu.DepthStencilState{Format: gputypes.TextureFormatDepth24Plus, DepthWriteEnabled: true, DepthCompare: gputypes.CompareFunctionLess},
			Multisample:  gputypes.DefaultMultisampleState(),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Do(func(*wgpu.Device) error {
		pipeline.Release()
		layout.Release()
		module.Release()
		return nil
	})
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		return f.RenderTo(tex, func(inner *Frame) error {
			if inner.Depth == nil {
				return errors.New("nested frame has no depth view")
			}
			pass, err := inner.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
				ColorAttachments: []wgpu.RenderPassColorAttachment{{
					View: inner.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore,
				}},
				DepthStencilAttachment: &wgpu.RenderPassDepthStencilAttachment{
					View: inner.Depth, DepthLoadOp: gputypes.LoadOpClear, DepthStoreOp: gputypes.StoreOpStore,
					DepthClearValue: 1, StencilReadOnly: true,
				},
			})
			if err != nil {
				return err
			}
			pass.SetPipeline(pipeline)
			pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 2})
			return pass.End()
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tex.ReadPixels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, got, solid(2, 2, color.NRGBA{R: 255, A: 255}))
}

func TestRenderToRejectsPlainTexture(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.UploadTexture(quadrants(), TextureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		if err := f.RenderTo(tex, clearTo(gputypes.Color{})); err == nil {
			return errors.New("RenderTo accepted a texture without RenderTarget")
		}
		return clearTo(gputypes.Color{})(f)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenderToRejectsNesting(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	outer, err := r.NewTexture(1, 1, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	inner, err := r.NewTexture(1, 1, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		return f.RenderTo(outer, func(o *Frame) error {
			if err := o.RenderTo(inner, clearTo(gputypes.Color{})); err == nil {
				return errors.New("nested RenderTo accepted")
			}
			return clearTo(gputypes.Color{})(o)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenderToClosedTexture(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.NewTexture(1, 1, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := tex.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		if err := f.RenderTo(tex, clearTo(gputypes.Color{})); !errors.Is(err, ErrClosed) {
			return errors.New("RenderTo on a closed texture did not report ErrClosed")
		}
		return clearTo(gputypes.Color{})(f)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenderToDrawErrorIsWrapped(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.NewTexture(1, 1, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	boom := errors.New("boom")
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		return f.RenderTo(tex, func(*Frame) error { return boom })
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
	// The renderer must still work afterwards.
	if _, _, err := r.Render(context.Background(), clearTo(gputypes.Color{})); err != nil {
		t.Fatal(err)
	}
}

func TestNewTextureReadPixelsNeedsAWrite(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.NewTexture(1, 1, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	if _, err := tex.ReadPixels(context.Background()); err == nil {
		t.Fatal("ReadPixels on a never-written texture succeeded")
	}
	if err := tex.Update(solid(1, 1, color.NRGBA{A: 255})); err != nil {
		t.Fatal(err)
	}
	got, err := tex.ReadPixels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	expectImage(t, got, solid(1, 1, color.NRGBA{A: 255}))
}

func TestRenderToRejectsNilAndForeignTextures(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	other := newTestRenderer(t, Options{Width: 1, Height: 1})
	foreign, err := other.NewTexture(1, 1, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		if err := f.RenderTo(nil, clearTo(gputypes.Color{})); err == nil || errors.Is(err, ErrClosed) {
			return fmt.Errorf("RenderTo(nil) = %v, want a non-ErrClosed error", err)
		}
		if err := f.RenderTo(foreign, clearTo(gputypes.Color{})); err == nil {
			return errors.New("RenderTo accepted a texture from another renderer")
		}
		return clearTo(gputypes.Color{})(f)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRenderToErrorFailsTheFrame: a draw callback that ignores a RenderTo
// error must not get its frame submitted, since the texture pass may have
// been left half recorded.
func TestRenderToErrorFailsTheFrame(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.NewTexture(1, 1, TextureOptions{RenderTarget: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tex.Close()
	boom := errors.New("boom")
	_, _, err = r.Render(context.Background(), func(f *Frame) error {
		_ = f.RenderTo(tex, func(inner *Frame) error {
			if err := clearTo(gputypes.Color{R: 1, A: 1})(inner); err != nil {
				return err
			}
			return boom
		})
		return clearTo(gputypes.Color{})(f)
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the swallowed RenderTo error", err)
	}
	if _, err := tex.ReadPixels(context.Background()); err == nil {
		t.Fatal("ReadPixels succeeded on a texture whose only pass was discarded")
	}
	if _, _, err := r.Render(context.Background(), clearTo(gputypes.Color{})); err != nil {
		t.Fatal(err)
	}
}

func TestRendererCloseReleasesRenderTargetWithDepth(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 1, Height: 1})
	tex, err := r.NewTexture(4, 4, TextureOptions{RenderTarget: true, Depth: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tex.Close(); err != nil {
		t.Fatal(err)
	}
}
