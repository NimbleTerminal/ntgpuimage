// Texture uploads a Go image, samples it in a custom shader, updates it, and
// writes both rendered frames as PNG files.
package main

import (
	"context"
	"flag"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
	"nimbleterminal.dev/ntgpuimage"
)

// The quad's UVs are listed rather than derived from its positions: naga
// v0.19.0 miscompiles arithmetic on an indexed array element for Metal.
const shader = `
@group(0) @binding(0) var tex: texture_2d<f32>;
@group(0) @binding(1) var samp: sampler;
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
}
@fragment fn fs_main(in: Out) -> @location(0) vec4<f32> {
	return textureSample(tex, samp, in.uv);
}`

func main() {
	first := flag.String("out", "texture.png", "PNG path for the uploaded image")
	second := flag.String("updated", "texture-updated.png", "PNG path for the updated image")
	flag.Parse()
	if err := run(*first, *second); err != nil {
		log.Fatal(err)
	}
}

func run(first, second string) error {
	ctx := context.Background()
	r, err := ntgpuimage.New(ctx, ntgpuimage.Options{Width: 256, Height: 256})
	if err != nil {
		return err
	}
	defer r.Close()

	// Nearest filtering keeps the 8x8 checkerboard's cells sharp when enlarged.
	tex, err := r.UploadTexture(checkerboard(color.NRGBA{R: 230, G: 120, B: 40, A: 255}), ntgpuimage.TextureOptions{Filter: gputypes.FilterModeNearest})
	if err != nil {
		return err
	}
	defer tex.Close()

	var module *wgpu.ShaderModule
	var layout *wgpu.PipelineLayout
	var pipeline *wgpu.RenderPipeline
	err = r.Do(func(dev *wgpu.Device) (err error) {
		if module, err = dev.CreateShaderModule(&wgpu.ShaderModuleDescriptor{WGSL: shader}); err != nil {
			return err
		}
		layout, err = dev.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{
			BindGroupLayouts: []*wgpu.BindGroupLayout{tex.BindGroupLayout()},
		})
		if err != nil {
			return err
		}
		pipeline, err = dev.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
			Layout: layout,
			Vertex: wgpu.VertexState{Module: module, EntryPoint: "vs_main"},
			Fragment: &wgpu.FragmentState{Module: module, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{{
				Format: gputypes.TextureFormatRGBA8Unorm, WriteMask: gputypes.ColorWriteMaskAll,
			}}},
			Primitive:   gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
			Multisample: gputypes.DefaultMultisampleState(),
		})
		return err
	})
	// Release on the renderer's thread, before the deferred Close.
	defer r.Do(func(*wgpu.Device) error {
		if pipeline != nil {
			pipeline.Release()
		}
		if layout != nil {
			layout.Release()
		}
		if module != nil {
			module.Release()
		}
		return nil
	})
	if err != nil {
		return err
	}

	draw := func(f *ntgpuimage.Frame) error {
		pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
			ColorAttachments: []wgpu.RenderPassColorAttachment{{
				View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore,
			}},
		})
		if err != nil {
			return err
		}
		pass.SetPipeline(pipeline)
		pass.SetBindGroup(0, tex.BindGroup(), nil)
		pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
		return pass.End()
	}

	img, _, err := r.Render(ctx, draw)
	if err != nil {
		return err
	}
	if err := writePNG(first, img); err != nil {
		return err
	}
	// Update keeps the texture and its bind group, so the pipeline is reused.
	if err := tex.Update(checkerboard(color.NRGBA{R: 40, G: 140, B: 230, A: 255})); err != nil {
		return err
	}
	if img, _, err = r.Render(ctx, draw); err != nil {
		return err
	}
	return writePNG(second, img)
}

func checkerboard(c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := range 8 {
		for x := range 8 {
			if (x+y)%2 == 0 {
				img.SetNRGBA(x, y, c)
			} else {
				img.SetNRGBA(x, y, color.NRGBA{R: 245, G: 245, B: 245, A: 255})
			}
		}
	}
	return img
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
