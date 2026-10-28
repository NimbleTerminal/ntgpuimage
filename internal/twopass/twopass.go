// Package twopass contains the shared native and browser two-pass example.
package twopass

import (
	"context"
	"fmt"
	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
	"image"
	"math"
	"nimbleterminal.dev/ntgpuimage"
)

// quad is a full-target triangle pair with explicit UVs. They are listed
// rather than derived from the positions: naga v0.19.0 miscompiles
// arithmetic on an indexed array element for Metal.
const quad = `
struct Out { @builtin(position) pos: vec4<f32>, @location(0) uv: vec2<f32> };
@vertex fn vs_main(@builtin(vertex_index) i: u32) -> Out {
	let pos = array<vec2<f32>, 6>(
		vec2<f32>(-1.0, 1.0), vec2<f32>(1.0, 1.0), vec2<f32>(1.0, -1.0),
		vec2<f32>(-1.0, 1.0), vec2<f32>(1.0, -1.0), vec2<f32>(-1.0, -1.0));
	let uv = array<vec2<f32>, 6>(
		vec2<f32>(0.0, 0.0), vec2<f32>(1.0, 0.0), vec2<f32>(1.0, 1.0),
		vec2<f32>(0.0, 0.0), vec2<f32>(1.0, 1.0), vec2<f32>(0.0, 1.0));
	var out: Out;
	out.pos = vec4<f32>(pos[i], 0.0, 1.0);
	out.uv = uv[i];
	return out;
}`

// patternShader needs no bindings: it paints rings from the UV alone.
const patternShader = quad + `
@fragment fn fs_main(in: Out) -> @location(0) vec4<f32> {
	let d = distance(in.uv, vec2<f32>(0.5, 0.5));
	let ring = 0.5 + 0.5 * sin(d * %.6f);
	return vec4<f32>(ring, in.uv.x, in.uv.y, 1.0);
}`

// sampleShader tiles the texture across the target.
const sampleShader = quad + `
@group(0) @binding(0) var tex: texture_2d<f32>;
@group(0) @binding(1) var samp: sampler;
@fragment fn fs_main(in: Out) -> @location(0) vec4<f32> {
	return textureSample(tex, samp, fract(in.uv * %d.0));
}`

// Options controls the procedural pattern and the intermediate texture.
type Options struct {
	Frequency  float64
	Tiles      int
	Resolution int
}

// Render records both GPU passes before reading either result back.
// The returned images are the final frame and the intermediate texture.
func Render(ctx context.Context, r *ntgpuimage.Renderer, opts Options) (*image.NRGBA, *image.NRGBA, error) {
	if math.IsNaN(opts.Frequency) || math.IsInf(opts.Frequency, 0) || opts.Frequency < 4 || opts.Frequency > 160 || opts.Tiles < 1 || opts.Tiles > 8 || opts.Resolution < 16 || opts.Resolution > 512 {
		return nil, nil, fmt.Errorf("two-pass options out of range")
	}
	// The texture is both a render target for the first pass and a sampled
	// image for the second. It is smaller than the main target to show that
	// the two need not match.
	tex, err := r.NewTexture(opts.Resolution, opts.Resolution, ntgpuimage.TextureOptions{RenderTarget: true})
	if err != nil {
		return nil, nil, err
	}
	defer tex.Close()

	pattern, err := newPipeline(r, fmt.Sprintf(patternShader, opts.Frequency), nil)
	if err != nil {
		return nil, nil, err
	}
	defer pattern.release(r)
	sample, err := newPipeline(r, fmt.Sprintf(sampleShader, opts.Tiles), tex.BindGroupLayout())
	if err != nil {
		return nil, nil, err
	}
	defer sample.release(r)

	img, _, err := r.Render(ctx, func(f *ntgpuimage.Frame) error {
		// Pass 1: paint the pattern into the texture. The nested Frame's
		// Color view is the texture, so the drawing code is the same as for
		// the main target.
		err := f.RenderTo(tex, func(t *ntgpuimage.Frame) error {
			return pattern.draw(t, nil)
		})
		if err != nil {
			return err
		}
		// Pass 2: sample the texture into the main target.
		return sample.draw(f, tex.BindGroup())
	})
	if err != nil {
		return nil, nil, err
	}
	// The intermediate texture can still be read back on its own.
	texImg, err := tex.ReadPixels(ctx)
	if err != nil {
		return nil, nil, err
	}
	return img, texImg, nil
}

// pipeline is a full-target quad with an optional single bind group.
type pipeline struct {
	module   *wgpu.ShaderModule
	layout   *wgpu.PipelineLayout
	pipeline *wgpu.RenderPipeline
}

func newPipeline(r *ntgpuimage.Renderer, wgsl string, group *wgpu.BindGroupLayout) (*pipeline, error) {
	p := &pipeline{}
	var groups []*wgpu.BindGroupLayout
	if group != nil {
		groups = []*wgpu.BindGroupLayout{group}
	}
	err := r.Do(func(dev *wgpu.Device) (err error) {
		if p.module, err = dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{WGSL: wgsl}); err != nil {
			return err
		}
		if p.layout, err = dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{BindGroupLayouts: groups}); err != nil {
			return err
		}
		p.pipeline, err = dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
			Layout: p.layout,
			Vertex: wgpu.VertexState{Module: p.module, EntryPoint: "vs_main"},
			Fragment: &wgpu.FragmentState{Module: p.module, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{{
				Format: gputypes.TextureFormatRGBA8Unorm, WriteMask: gputypes.ColorWriteMaskAll,
			}}},
			Primitive:   gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
			Multisample: gputypes.DefaultMultisampleState(),
		})
		return err
	})
	if err != nil {
		p.release(r)
		return nil, err
	}
	return p, nil
}

// release runs on the renderer's thread; call it before the renderer closes.
func (p *pipeline) release(r *ntgpuimage.Renderer) {
	_ = r.Do(func(*wgpu.Device) error {
		if p.pipeline != nil {
			p.pipeline.Release()
		}
		if p.layout != nil {
			p.layout.Release()
		}
		if p.module != nil {
			p.module.Release()
		}
		return nil
	})
}

// draw clears f.Color and draws the quad, binding group at index 0 if given.
func (p *pipeline) draw(f *ntgpuimage.Frame, group *wgpu.BindGroup) error {
	pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
		ColorAttachments: []wgpu.RenderPassColorAttachment{{
			View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore,
		}},
	})
	if err != nil {
		return err
	}
	pass.SetPipeline(p.pipeline)
	if group != nil {
		pass.SetBindGroup(0, group, nil)
	}
	pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
	return pass.End()
}
