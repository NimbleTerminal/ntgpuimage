package ntgpuimage

import (
	"context"
	"errors"
	"image"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

// GPU mip generation for render targets.
//
// After Frame.RenderTo draws level 0 of a mipmapped render target, each
// lower level is drawn by one render pass that samples the level above it
// with a linear, clamping sampler at the destination texel centers. When a
// level halves exactly, each tap sits on the corner shared by four source
// texels, so the bilinear sample is their 2×2 box average. When a source
// dimension is odd the tap still lands at the destination texel's center,
// so it blends the two or four nearest source texels with the bilinear
// weights that position gives and never reads past the edge. An SRGB
// target is sampled through its sRGB view, so the average is taken in
// linear light and encoded again on write, like MipChain with SRGB; alpha
// is not weighted into RGB, like MipChain without AlphaWeighted.
//
// Each level is drawn into a one-level scratch texture of its own size and
// then copied into the mip level, rather than attached through a mip view.
// In gogpu v0.34.5 every texture view reports the base texture's size: the
// Vulkan backend sizes a pass's framebuffer from that, and the core layer
// compares attachments with it, so a mip view cannot be a correct render
// attachment on every backend. The scratch textures persist with the
// texture and together hold about a third of level 0.
//
// gogpu's submit-time usage tracker records one usage per texture per
// submission and, before the command buffer runs, records a whole-texture
// barrier from the usage it holds to the new one unless both are the same
// hardware-ordered usage. Copy destination is not ordered, so a frame that
// ended with the level copies would make every later generating frame start
// with a TRANSFER_DST→TRANSFER_DST barrier on a texture that actually rests
// in the sampled layout. Each generation therefore ends by copying one texel
// of the last level into a renderer-owned sink buffer: copy source is
// ordered, so the tracker holds CopySrc for the texture from its first pass
// on and later generating frames merge without a barrier, and the explicit
// per-level transitions here describe the real layouts. A second render
// pass on the texture in the same frame would conflict with the copy usages
// inside gogpu, so RenderTo accepts a mipmapped render target once per frame.

// downsampleShader draws the full target, sampling level 0 of the bound
// view (one level of the chain) at the destination texel centers. UVs are
// listed explicitly for the same naga reason as blitShader.
const downsampleShader = `
@group(0) @binding(0) var tex: texture_2d<f32>;
@group(0) @binding(1) var samp: sampler;
struct Out { @builtin(position) pos: vec4<f32>, @location(0) uv: vec2<f32> };
@vertex fn vs_main(@builtin(vertex_index) i: u32) -> Out {
	let p = array<vec2<f32>, 6>(
		vec2<f32>(-1.0, 1.0), vec2<f32>(1.0, 1.0), vec2<f32>(1.0, -1.0),
		vec2<f32>(-1.0, 1.0), vec2<f32>(1.0, -1.0), vec2<f32>(-1.0, -1.0));
	let uv = array<vec2<f32>, 6>(
		vec2<f32>(0.0, 0.0), vec2<f32>(1.0, 0.0), vec2<f32>(1.0, 1.0),
		vec2<f32>(0.0, 0.0), vec2<f32>(1.0, 1.0), vec2<f32>(0.0, 1.0));
	var o: Out;
	o.pos = vec4<f32>(p[i], 0.0, 1.0);
	o.uv = uv[i];
	return o;
}
@fragment fn fs_main(o: Out) -> @location(0) vec4<f32> {
	return textureSampleLevel(tex, samp, o.uv, 0.0);
}`

// levelSize returns the dimensions of a mip level.
func levelSize(w, h, level int) (int, int) {
	for range level {
		w, h = max(1, w/2), max(1, h/2)
	}
	return w, h
}

// transitionLevel is transition for one mip level.
func transitionLevel(enc *wgpu.CommandEncoder, tex *wgpu.Texture, level int, from, to gputypes.TextureUsage) {
	enc.TransitionTextures([]wgpu.TextureBarrier{{
		Texture: tex,
		Range:   wgpu.TextureRange{Aspect: gputypes.TextureAspectAll, BaseMipLevel: uint32(level), MipLevelCount: 1, ArrayLayerCount: 1},
		Usage:   wgpu.TextureUsageTransition{OldUsage: from, NewUsage: to},
	}})
}

// sampledPipelineLayout returns the pipeline layout with only the sampled
// image bind group, shared by the readback blit and mip generation. It runs
// on the executor; the layout is released with the device.
func (r *Renderer) sampledPipelineLayout() (*wgpu.PipelineLayout, error) {
	if r.blitLayout != nil {
		return r.blitLayout, nil
	}
	if r.sampledLayout == nil {
		return nil, &Error{Op: "create sampled pipeline layout", Err: errors.New("no sampled texture layout")}
	}
	layout, err := r.device.CreatePipelineLayout(&wgpu.PipelineLayoutDescriptor{Label: r.label + ":sampled-image", BindGroupLayouts: []*wgpu.BindGroupLayout{r.sampledLayout}})
	if err != nil {
		return nil, &Error{Op: "create sampled pipeline layout", Err: err}
	}
	r.blitLayout = layout
	return layout, nil
}

// mipSamplerHandle returns the linear, clamping, single-level sampler that
// mip generation reads with, creating it on first use on the executor.
func (r *Renderer) mipSamplerHandle() (*wgpu.Sampler, error) {
	if r.mipSampler != nil {
		return r.mipSampler, nil
	}
	sampler, err := r.device.CreateSampler(&wgpu.SamplerDescriptor{
		Label: r.label + ":mip-sampler", AddressModeU: gputypes.AddressModeClampToEdge, AddressModeV: gputypes.AddressModeClampToEdge,
		MagFilter: gputypes.FilterModeLinear, MinFilter: gputypes.FilterModeLinear, MipmapFilter: gputypes.FilterModeNearest, Anisotropy: 1,
	})
	if err != nil {
		return nil, &Error{Op: "create mip sampler", Err: err}
	}
	r.mipSampler = sampler
	return sampler, nil
}

// downsamplePipeline returns the mip generation pipeline writing format,
// creating it on first use on the executor.
func (r *Renderer) downsamplePipeline(format gputypes.TextureFormat) (*wgpu.RenderPipeline, error) {
	if p := r.mips[format]; p != nil {
		return p, nil
	}
	layout, err := r.sampledPipelineLayout()
	if err != nil {
		return nil, err
	}
	if r.mipModule == nil {
		module, err := r.device.CreateShaderModule(&wgpu.ShaderModuleDescriptor{Label: r.label + ":mip-downsample", WGSL: downsampleShader})
		if err != nil {
			return nil, &Error{Op: "create mip shader", Err: err}
		}
		r.mipModule = module
	}
	pipeline, err := r.device.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
		Label:       r.label + ":mip-downsample",
		Layout:      layout,
		Vertex:      wgpu.VertexState{Module: r.mipModule, EntryPoint: "vs_main"},
		Fragment:    &wgpu.FragmentState{Module: r.mipModule, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{{Format: format, WriteMask: gputypes.ColorWriteMaskAll}}},
		Primitive:   gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
		Multisample: gputypes.DefaultMultisampleState(),
	})
	if err != nil {
		return nil, &Error{Op: "create mip pipeline", Err: err}
	}
	if r.mips == nil {
		r.mips = make(map[gputypes.TextureFormat]*wgpu.RenderPipeline)
	}
	r.mips[format] = pipeline
	return pipeline, nil
}

// allocateMipTargets creates, for a mipmapped render target, one view and
// one sampled bind group per level, and one scratch target per level below
// 0. It runs inside allocateTexture, which releases t on error.
func (t *Texture) allocateMipTargets() error {
	r := t.r
	sampler, err := r.mipSamplerHandle()
	if err != nil {
		return err
	}
	t.levelViews = make([]*wgpu.TextureView, t.mipLevels)
	t.levelGroups = make([]*wgpu.BindGroup, t.mipLevels)
	t.mipScratch = make([]*wgpu.Texture, t.mipLevels)
	t.mipScratchViews = make([]*wgpu.TextureView, t.mipLevels)
	for level := range t.mipLevels {
		t.levelViews[level], err = r.device.CreateTextureView(t.texture, &wgpu.TextureViewDescriptor{
			Label: r.label + ":mip-level", BaseMipLevel: uint32(level), MipLevelCount: 1, ArrayLayerCount: 1,
		})
		if err != nil {
			return &Error{Op: "create mip level view", Err: err}
		}
		t.levelGroups[level], err = r.device.CreateBindGroup(&wgpu.BindGroupDescriptor{
			Label: r.label + ":mip-level-group", Layout: r.sampledLayout,
			Entries: []wgpu.BindGroupEntry{{Binding: 0, TextureView: t.levelViews[level]}, {Binding: 1, Sampler: sampler}},
		})
		if err != nil {
			return &Error{Op: "create mip level group", Err: err}
		}
		if level == 0 {
			continue
		}
		w, h := levelSize(t.width, t.height, level)
		t.mipScratch[level], err = r.device.CreateTexture(&wgpu.TextureDescriptor{
			Label:         r.label + ":mip-scratch",
			Size:          wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
			MipLevelCount: 1,
			SampleCount:   1,
			Dimension:     gputypes.TextureDimension2D,
			Format:        t.format,
			Usage:         gputypes.TextureUsageRenderAttachment | gputypes.TextureUsageCopySrc,
		})
		if err != nil {
			return &Error{Op: "create mip scratch texture", Err: err}
		}
		t.mipScratchViews[level], err = r.device.CreateTextureView(t.mipScratch[level], nil)
		if err != nil {
			return &Error{Op: "create mip scratch view", Err: err}
		}
	}
	return nil
}

func (t *Texture) releaseMipTargets() {
	for _, g := range t.levelGroups {
		if g != nil {
			g.Release()
		}
	}
	for _, v := range t.levelViews {
		if v != nil {
			v.Release()
		}
	}
	for _, v := range t.mipScratchViews {
		if v != nil {
			v.Release()
		}
	}
	for _, tex := range t.mipScratch {
		if tex != nil {
			tex.Release()
		}
	}
	t.levelGroups, t.levelViews, t.mipScratchViews, t.mipScratch = nil, nil, nil, nil
}

// generatesMips reports whether RenderTo, and Update where allowed, fill this
// texture's chain on the GPU: every render target with more than one level,
// whether the chain was allocated by Mipmaps or supplied to UploadTextureLevels.
func (t *Texture) generatesMips() bool { return t.renderTarget && t.mipLevels > 1 }

// mipSinkBuffer returns the buffer that generateMips copies one texel into,
// creating it on first use on the executor. One padded row is the smallest
// texture-to-buffer copy.
func (r *Renderer) mipSinkBuffer() (*wgpu.Buffer, error) {
	if r.mipSink != nil {
		return r.mipSink, nil
	}
	sink, err := r.device.CreateBuffer(&wgpu.BufferDescriptor{Label: r.label + ":mip-sink", Size: rowAlign, Usage: wgpu.BufferUsageCopyDst})
	if err != nil {
		return nil, &Error{Op: "create mip sink buffer", Err: err}
	}
	r.mipSink = sink
	return sink, nil
}

// generateMips records, for each level below 0, a pass into that level's
// scratch target and a copy into the level, then the one-texel copy into the
// sink buffer that settles the usage tracker. Level 0 must already be in the
// sampled layout; each generated level is left there too. Levels below 0
// are in the sampled layout when they have been written before (t.written),
// and undefined otherwise. On failure the encoder may hold a partial pass
// and must be discarded.
func (t *Texture) generateMips(enc *wgpu.CommandEncoder) error {
	pipeline, err := t.r.downsamplePipeline(t.format)
	if err != nil {
		return err
	}
	sink, err := t.r.mipSinkBuffer()
	if err != nil {
		return err
	}
	var previous gputypes.TextureUsage
	if t.written {
		previous = gputypes.TextureUsageTextureBinding
	}
	for level := 1; level < t.mipLevels; level++ {
		w, h := levelSize(t.width, t.height, level)
		// LoadOp Clear starts the scratch pass from an undefined layout, so the
		// scratch needs no transition back from its copy-source layout.
		pass, err := enc.BeginRenderPass(&wgpu.RenderPassDescriptor{
			Label:            t.r.label + ":mip-downsample",
			ColorAttachments: []wgpu.RenderPassColorAttachment{{View: t.mipScratchViews[level], LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore}},
		})
		if err != nil {
			return &Error{Op: "begin mip pass", Err: err}
		}
		pass.SetPipeline(pipeline)
		pass.SetBindGroup(0, t.levelGroups[level-1], nil)
		pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
		if err := pass.End(); err != nil {
			return &Error{Op: "end mip pass", Err: err}
		}
		transition(enc, t.mipScratch[level], gputypes.TextureUsageRenderAttachment, gputypes.TextureUsageCopySrc)
		transitionLevel(enc, t.texture, level, previous, gputypes.TextureUsageCopyDst)
		enc.CopyTextureToTexture(t.mipScratch[level], t.texture, []wgpu.TextureCopy{{
			Source:      wgpu.ImageCopyTexture{Texture: t.mipScratch[level]},
			Destination: wgpu.ImageCopyTexture{Texture: t.texture, MipLevel: uint32(level)},
			Size:        wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
		}})
		// The next pass samples this level, and callers sample the chain.
		transitionLevel(enc, t.texture, level, gputypes.TextureUsageCopyDst, gputypes.TextureUsageTextureBinding)
	}
	last := t.mipLevels - 1
	transitionLevel(enc, t.texture, last, gputypes.TextureUsageTextureBinding, gputypes.TextureUsageCopySrc)
	enc.CopyTextureToBuffer(t.texture, sink, []wgpu.BufferTextureCopy{{
		BufferLayout: wgpu.ImageDataLayout{BytesPerRow: rowAlign, RowsPerImage: 1},
		TextureBase:  wgpu.ImageCopyTexture{Texture: t.texture, MipLevel: uint32(last)},
		Size:         wgpu.Extent3D{Width: 1, Height: 1, DepthOrArrayLayers: 1},
	}})
	transitionLevel(enc, t.texture, last, gputypes.TextureUsageCopySrc, gputypes.TextureUsageTextureBinding)
	return nil
}

// regenerateMips fills the chain from level 0 in its own submission, after
// Update has written level 0. It runs on the executor.
func (t *Texture) regenerateMips() error {
	r := t.r
	enc, err := r.device.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: r.label + ":mip-regenerate"})
	if err != nil {
		return &Error{Op: "create encoder", Err: err}
	}
	if err := t.generateMips(enc); err != nil {
		enc.DiscardEncoding()
		return err
	}
	cmd, err := r.submit(enc)
	if err != nil {
		return err
	}
	r.device.Poll(wgpu.PollWait)
	r.device.FreeCommandBuffer(cmd)
	return nil
}

// readLevel reads one level back through the blit path, for tests. Levels
// above 0 need the per-level bind groups of a mipmapped render target.
func (t *Texture) readLevel(ctx context.Context, level int) (*image.NRGBA, error) {
	if t.r.closed.Load() {
		return nil, ErrClosed
	}
	var img *image.NRGBA
	var err error
	if runErr := t.r.exec.run(ctx, func() {
		r := t.r
		if t.closed || r.device == nil {
			err = ErrClosed
			return
		}
		if level < 0 || level >= t.mipLevels || (level > 0 && t.levelGroups == nil) {
			err = &Error{Op: "read level", Err: errors.New("no such readable level")}
			return
		}
		if !t.written {
			err = &Error{Op: "read level", Err: errors.New("nothing has been written to or drawn into the texture")}
			return
		}
		group := t.group
		if level > 0 {
			group = t.levelGroups[level]
		}
		w, h := levelSize(t.width, t.height, level)
		stride := alignedBytesPerRow(w)
		var staging *wgpu.Buffer
		staging, err = r.device.CreateBuffer(&wgpu.BufferDescriptor{Label: r.label + ":level-readback", Size: uint64(stride * h), Usage: wgpu.BufferUsageCopyDst | wgpu.BufferUsageMapRead})
		if err != nil {
			err = &Error{Op: "create level readback buffer", Err: err}
			return
		}
		defer staging.Release()
		var enc *wgpu.CommandEncoder
		enc, err = r.device.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: r.label + ":level-readback"})
		if err != nil {
			err = &Error{Op: "create encoder", Err: err}
			return
		}
		img, err = t.readByBlit(ctx, enc, staging, stride, group, w, h)
	}); runErr != nil {
		return nil, runErr
	}
	return img, err
}
