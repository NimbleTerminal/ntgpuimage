// Twopass draws a procedural pattern into a GPU texture with one shader,
// then samples that texture in a second shader to fill the main target,
// all in one frame and without reading pixels back in between. It writes the
// final frame and the intermediate texture as PNG files.
package main

import (
	"context"
	"flag"
	"image"
	"image/png"
	"log"
	"os"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
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
	let ring = 0.5 + 0.5 * sin(d * 60.0);
	return vec4<f32>(ring, in.uv.x, in.uv.y, 1.0);
}`

// sampleShader tiles the texture twice across the target.
const sampleShader = quad + `
@group(0) @binding(0) var tex: texture_2d<f32>;
@group(0) @binding(1) var samp: sampler;
@fragment fn fs_main(in: Out) -> @location(0) vec4<f32> {
	return textureSample(tex, samp, fract(in.uv * 2.0));
}`

func main() {
	out := flag.String("out", "twopass.png", "PNG path for the final frame")
	texOut := flag.String("texture", "twopass-texture.png", "PNG path for the intermediate texture")
	flag.Parse()
	if err := run(*out, *texOut); err != nil {
		log.Fatal(err)
	}
}

func run(out, texOut string) error {
	ctx := context.Background()
	r, err := ntgpuimage.New(ctx, ntgpuimage.Options{Width: 256, Height: 256})
	if err != nil {
		return err
	}
	defer r.Close()

	// The texture is both a render target for the first pass and a sampled
	// image for the second. It is smaller than the main target to show that
	// the two need not match.
	tex, err := r.NewTexture(128, 128, ntgpuimage.TextureOptions{RenderTarget: true})
	if err != nil {
		return err
	}
	defer tex.Close()

	pattern, err := newPipeline(r, patternShader, nil)
	if err != nil {
		return err
	}
	defer pattern.release(r)
	sample, err := newPipeline(r, sampleShader, tex.BindGroupLayout())
	if err != nil {
		return err
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
		return err
	}
	if err := writePNG(out, img); err != nil {
		return err
	}
	// The intermediate texture can still be read back on its own.
	texImg, err := tex.ReadPixels(ctx)
	if err != nil {
		return err
	}
	return writePNG(texOut, texImg)
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

func writePNG(path string, img image.Image) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, img); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
