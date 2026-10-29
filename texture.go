package ntgpuimage

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

// Texture is an image owned by a Renderer, stored as RGBA8Unorm unless
// TextureOptions selects another format. It can be sampled in a draw
// callback, updated without reallocating, or copied back to Go pixels.
// Binding 0 is texture_2d<f32>; binding 1 is a filtering sampler. UVs clamp
// at the edges unless TextureOptions selects repeat or mirror addressing.
type Texture struct {
	r             *Renderer
	mipLevels     int
	mipOptions    MipOptions
	autoMips      bool
	format        gputypes.TextureFormat
	texture       *wgpu.Texture
	view          *wgpu.TextureView
	sampler       *wgpu.Sampler
	group         *wgpu.BindGroup
	depthTex      *wgpu.Texture     // nil unless TextureOptions.Depth
	depthView     *wgpu.TextureView // nil unless TextureOptions.Depth
	width, height int
	renderTarget  bool

	externalEncoder *wgpu.CommandEncoder // last host encoder used for mip generation

	// colorView is the view RenderTo attaches: level 0 alone for a mipmapped
	// render target, otherwise view.
	colorView *wgpu.TextureView
	// The fields below exist only for a mipmapped render target; see
	// mipgen_target.go. levelGroups sample one level each through the
	// renderer's mip sampler; mipScratch holds the target each level is
	// drawn into before it is copied; mipFrame is the last frame that
	// regenerated the chain.
	levelViews      []*wgpu.TextureView
	levelGroups     []*wgpu.BindGroup
	mipScratch      []*wgpu.Texture
	mipScratchViews []*wgpu.TextureView
	mipFrame        uint64

	// The fields below are accessed only on the renderer executor.
	closed bool
	// written is set once pixels have been uploaded or a RenderTo pass has
	// completed, so ReadPixels can refuse a texture with undefined contents.
	written bool
	// drawn is set once a RenderTo pass has completed. gogpu's usage tracker
	// then holds the attachment usage for good, and ReadPixels must not copy
	// from the texture directly; see readPixelsByBlit.
	drawn bool
	// mipsFilled is set once every level below 0 holds supplied or generated
	// texels and rests in the sampled layout. A first Update writes only
	// level 0, so generateMips must then start the lower levels from the
	// undefined layout rather than the sampled one.
	mipsFilled bool
}

// TextureOptions configures UploadTexture and NewTexture.
type TextureOptions struct {
	// Filter selects the sampling filter. Zero selects linear filtering.
	// Nearest filtering is useful for pixel art or an atlas without padded
	// tiles.
	Filter gputypes.FilterMode
	// Anisotropy sets the sampler's maximum anisotropy. Zero or 1 disables it;
	// 2 through 16 improves texture detail at grazing angles. Values above 16
	// and anisotropy above 1 with nearest Filter are rejected. It applies to
	// every texture, including render targets, independently of Mipmaps.
	Anisotropy uint16
	// AddressModeU and AddressModeV select clamp, repeat, or mirror-repeat.
	// Zero selects ClampToEdge on each axis independently.
	AddressModeU gputypes.AddressMode
	AddressModeV gputypes.AddressMode
	// Mipmaps allocates the full mip chain. UploadTexture and Update generate
	// it on the CPU using MipChain. For a RenderTarget, including Float16,
	// every Frame.RenderTo and Update instead regenerates the chain on the
	// GPU from level 0 with a bilinear 2×2 reduction in the texture's color
	// space; see mipgen_target.go. A Depth attachment keeps one level.
	// Linear Filter blends mip levels (trilinear filtering); nearest Filter
	// selects the nearest level.
	Mipmaps bool
	// MipAlphaWeighted enables alpha-weighted RGB when generating mipmaps.
	// It does not preserve alpha-test coverage; supply adjusted levels for cutouts.
	MipAlphaWeighted bool
	// RenderTarget also allows the texture to be drawn into with
	// Frame.RenderTo. A render target keeps its fixed size: to change it,
	// create a new texture and close the old one.
	RenderTarget bool
	// Depth allocates a Depth24Plus attachment alongside the color texture,
	// exposed as Frame.Depth inside Frame.RenderTo. It requires RenderTarget.
	Depth bool
	// SRGB stores the texture as RGBA8UnormSrgb. Its bytes are still
	// sRGB-encoded: UploadTexture, Update and ReadPixels exchange the same
	// bytes as for a plain texture. Only shaders see a difference. Sampling
	// decodes each texel to linear light before filtering, so shaders
	// receive linear values. A RenderTarget encodes what fragment shaders
	// write and blends in linear light, so shaders output linear color,
	// and pipelines that draw into it target TextureFormatRGBA8UnormSrgb.
	// Alpha is never encoded.
	SRGB bool
	// Float16 stores the texture as RGBA16Float, for linear-light or other
	// intermediate results that need more than 8 bits or values outside
	// [0, 1]. It requires RenderTarget, excludes SRGB, and is written only by
	// Frame.RenderTo: UploadTexture and Update reject it. ReadPixels clamps
	// it to [0, 1] and rounds to 8 bits. Shaders can read it with
	// textureSample or textureLoad through BindGroup.
	Float16 bool
}

func (o TextureOptions) validate(op string) error {
	if o.Anisotropy > 16 {
		return &Error{Op: op, Err: fmt.Errorf("anisotropy %d exceeds maximum 16", o.Anisotropy)}
	}
	if o.Anisotropy > 1 && o.Filter == gputypes.FilterModeNearest {
		return &Error{Op: op, Err: errors.New("anisotropy above 1 requires linear filtering")}
	}
	for _, mode := range []gputypes.AddressMode{o.AddressModeU, o.AddressModeV} {
		if mode != 0 && mode != gputypes.AddressModeClampToEdge && mode != gputypes.AddressModeRepeat && mode != gputypes.AddressModeMirrorRepeat {
			return &Error{Op: op, Err: fmt.Errorf("invalid address mode %v", mode)}
		}
	}
	if o.Depth && !o.RenderTarget {
		return &Error{Op: op, Err: errors.New("Depth requires RenderTarget")}
	}
	if o.Float16 && !o.RenderTarget {
		return &Error{Op: op, Err: errors.New("Float16 requires RenderTarget")}
	}
	if o.Float16 && o.SRGB {
		return &Error{Op: op, Err: errors.New("Float16 and SRGB are exclusive")}
	}
	return nil
}

// format returns the storage format that validated options select.
func (o TextureOptions) format() gputypes.TextureFormat {
	switch {
	case o.SRGB:
		return gputypes.TextureFormatRGBA8UnormSrgb
	case o.Float16:
		return gputypes.TextureFormatRGBA16Float
	}
	return gputypes.TextureFormatRGBA8Unorm
}

// filter returns the sampler filter to use, or an error for an unknown one.
func (o TextureOptions) filter(op string) (gputypes.FilterMode, error) {
	filter := o.Filter
	if filter == 0 {
		filter = gputypes.FilterModeLinear
	}
	if filter != gputypes.FilterModeLinear && filter != gputypes.FilterModeNearest {
		return 0, &Error{Op: op, Err: fmt.Errorf("invalid filter %v", filter)}
	}
	return filter, nil
}

// UploadTexture copies an image into a sampled GPU texture. The source image
// can have any bounds or image.Image implementation; pixels are copied before
// this method returns. Close the Texture when it is no longer needed, or let
// Renderer.Close release it with the device.
func (r *Renderer) UploadTexture(src image.Image, opts TextureOptions) (*Texture, error) {
	const op = "upload texture"
	if r.closed.Load() {
		return nil, ErrClosed
	}
	if err := opts.validate(op); err != nil {
		return nil, err
	}
	if opts.Float16 {
		return nil, &Error{Op: op, Err: errors.New("a Float16 texture cannot be uploaded; draw it with Frame.RenderTo")}
	}
	filter, err := opts.filter(op)
	if err != nil {
		return nil, err
	}
	// Reject an unusable image from its bounds, before copying its pixels.
	w, h, err := imageSize(op, src)
	if err != nil {
		return nil, err
	}
	if err := r.checkTextureSize(op, w, h); err != nil {
		return nil, err
	}
	pixels, err := uploadPixels(src, opts.Mipmaps, MipOptions{SRGB: opts.SRGB, AlphaWeighted: opts.MipAlphaWeighted})
	if err != nil {
		return nil, err
	}
	count := 1
	if opts.Mipmaps {
		count = mipLevelCount(w, h)
	}
	return r.createTexture(pixels, w, h, filter, opts, count)
}

// NewTexture allocates a w×h texture with undefined contents. Write it with
// Update or, for a RenderTarget, with Frame.RenderTo before sampling or
// reading it. Close the Texture when it is no longer needed, or let
// Renderer.Close release it with the device.
func (r *Renderer) NewTexture(w, h int, opts TextureOptions) (*Texture, error) {
	const op = "new texture"
	if r.closed.Load() {
		return nil, ErrClosed
	}
	if err := opts.validate(op); err != nil {
		return nil, err
	}
	filter, err := opts.filter(op)
	if err != nil {
		return nil, err
	}
	if w <= 0 || h <= 0 {
		return nil, &Error{Op: op, Err: fmt.Errorf("invalid size %dx%d", w, h)}
	}
	if err := r.checkTextureSize(op, w, h); err != nil {
		return nil, err
	}
	count := 1
	if opts.Mipmaps {
		count = mipLevelCount(w, h)
	}
	return r.createTexture(nil, w, h, filter, opts, count)
}

// createTexture runs allocateTexture on the executor. A nil pixels leaves the
// texture unwritten.
func (r *Renderer) createTexture(pixels [][]byte, w, h int, filter gputypes.FilterMode, opts TextureOptions, mipLevels int) (*Texture, error) {
	var (
		result *Texture
		err    error
	)
	if runErr := r.exec.run(context.Background(), func() {
		if r.device == nil {
			err = ErrClosed
			return
		}
		result, err = r.allocateTexture(pixels, w, h, filter, opts, mipLevels)
	}); runErr != nil {
		return nil, runErr
	}
	return result, err
}

// imageSize validates an image's bounds without reading its pixels.
func imageSize(op string, src image.Image) (w, h int, err error) {
	if src == nil {
		return 0, 0, &Error{Op: op, Err: fmt.Errorf("nil image")}
	}
	b := src.Bounds()
	w, h = b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w > int(^uint(0)>>1)/4/h {
		return 0, 0, &Error{Op: op, Err: fmt.Errorf("invalid image size %dx%d", w, h)}
	}
	return w, h, nil
}

// packPixels copies an image that imageSize accepted into tightly packed
// straight-alpha RGBA rows.
func packPixels(src image.Image) []byte {
	b := src.Bounds()
	packed := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(packed, packed.Bounds(), src, b.Min, draw.Src)
	return packed.Pix
}

func (r *Renderer) allocateTexture(pixels [][]byte, w, h int, filter gputypes.FilterMode, opts TextureOptions, mipLevels int) (_ *Texture, err error) {
	if r.sampledLayout == nil {
		r.sampledLayout, err = r.device.CreateBindGroupLayout(&wgpu.BindGroupLayoutDescriptor{
			Label: r.label + ":sampled-image",
			Entries: []gputypes.BindGroupLayoutEntry{
				{Binding: 0, Visibility: gputypes.ShaderStageFragment, Texture: &gputypes.TextureBindingLayout{SampleType: gputypes.TextureSampleTypeFloat, ViewDimension: gputypes.TextureViewDimension2D}},
				{Binding: 1, Visibility: gputypes.ShaderStageFragment, Sampler: &gputypes.SamplerBindingLayout{Type: gputypes.SamplerBindingTypeFiltering}},
			},
		})
		if err != nil {
			return nil, &Error{Op: "create sampled image layout", Err: err}
		}
	}
	t := &Texture{
		r: r, format: opts.format(), width: w, height: h,
		mipLevels: mipLevels, autoMips: opts.Mipmaps,
		mipOptions:   MipOptions{SRGB: opts.SRGB, AlphaWeighted: opts.MipAlphaWeighted},
		renderTarget: opts.RenderTarget,
	}
	defer func() {
		if err != nil {
			t.release()
		}
	}()
	size := wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1}
	usage := gputypes.TextureUsageTextureBinding | gputypes.TextureUsageCopyDst | gputypes.TextureUsageCopySrc
	if opts.RenderTarget {
		usage |= gputypes.TextureUsageRenderAttachment
	}
	t.texture, err = r.device.CreateTexture(&wgpu.TextureDescriptor{
		Label:         r.label + ":sampled-image",
		Size:          size,
		MipLevelCount: uint32(mipLevels),
		SampleCount:   1,
		Dimension:     gputypes.TextureDimension2D,
		Format:        t.format,
		Usage:         usage,
	})
	if err != nil {
		return nil, &Error{Op: "create sampled texture", Err: err}
	}
	t.view, err = r.device.CreateTextureView(t.texture, nil)
	if err != nil {
		return nil, &Error{Op: "create sampled texture view", Err: err}
	}
	t.colorView = t.view
	if t.generatesMips() {
		if err = t.allocateMipTargets(); err != nil {
			return nil, err
		}
		t.colorView = t.levelViews[0]
	}
	if opts.Depth {
		t.depthTex, err = r.device.CreateTexture(&wgpu.TextureDescriptor{
			Label:         r.label + ":sampled-image-depth",
			Size:          size,
			MipLevelCount: 1,
			SampleCount:   1,
			Dimension:     gputypes.TextureDimension2D,
			Format:        gputypes.TextureFormatDepth24Plus,
			Usage:         gputypes.TextureUsageRenderAttachment,
		})
		if err != nil {
			return nil, &Error{Op: "create sampled texture depth", Err: err}
		}
		t.depthView, err = r.device.CreateTextureView(t.depthTex, nil)
		if err != nil {
			return nil, &Error{Op: "create sampled texture depth view", Err: err}
		}
	}
	addressU, addressV := opts.AddressModeU, opts.AddressModeV
	if addressU == 0 {
		addressU = gputypes.AddressModeClampToEdge
	}
	if addressV == 0 {
		addressV = gputypes.AddressModeClampToEdge
	}
	t.sampler, err = r.device.CreateSampler(&wgpu.SamplerDescriptor{
		Label: r.label + ":linear-sampler", AddressModeU: addressU, AddressModeV: addressV,
		MagFilter: filter, MinFilter: filter, MipmapFilter: filter, LodMaxClamp: float32(mipLevels - 1), Anisotropy: max(1, opts.Anisotropy),
	})
	if err != nil {
		return nil, &Error{Op: "create sampled texture sampler", Err: err}
	}
	t.group, err = r.device.CreateBindGroup(&wgpu.BindGroupDescriptor{
		Label: r.label + ":sampled-image-group", Layout: r.sampledLayout,
		Entries: []wgpu.BindGroupEntry{{Binding: 0, TextureView: t.view}, {Binding: 1, Sampler: t.sampler}},
	})
	if err != nil {
		return nil, &Error{Op: "create sampled texture group", Err: err}
	}
	if pixels != nil {
		if err = t.writeLevels(pixels); err != nil {
			return nil, err
		}
	}
	if r.textures == nil {
		r.textures = make(map[*Texture]struct{})
	}
	r.textures[t] = struct{}{}
	return t, nil
}

func (t *Texture) writeLevels(pixels [][]byte) error {
	w, h := t.width, t.height
	for level, data := range pixels {
		if err := t.r.device.Queue().WriteTexture(
			&wgpu.ImageCopyTexture{Texture: t.texture, MipLevel: uint32(level)}, data,
			&wgpu.ImageDataLayout{BytesPerRow: uint32(w * 4), RowsPerImage: uint32(h)},
			&wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
		); err != nil {
			return &Error{Op: "write sampled texture", Err: err}
		}
		w, h = max(1, w/2), max(1, h/2)
	}
	t.written = true
	if len(pixels) == t.mipLevels {
		t.mipsFilled = true
	}
	return nil
}

// MipLevelCount returns the immutable number of allocated mip levels.
func (t *Texture) MipLevelCount() int { return t.mipLevels }

// Size returns the immutable texture dimensions in pixels.
func (t *Texture) Size() (int, int) { return t.width, t.height }

// Format returns the immutable storage format: RGBA8Unorm by default,
// RGBA8UnormSrgb with TextureOptions.SRGB, or RGBA16Float with
// TextureOptions.Float16. A pipeline that draws into a RenderTarget names it
// as its color target format.
func (t *Texture) Format() gputypes.TextureFormat { return t.format }

// BindGroupLayout is the layout to include in a caller's pipeline layout.
// Use BindGroup at the matching group index inside Renderer.Render/Do.
// These raw GPU handles are valid only while the Texture and Renderer are open.
func (t *Texture) BindGroupLayout() *wgpu.BindGroupLayout { return t.r.sampledLayout }
func (t *Texture) BindGroup() *wgpu.BindGroup             { return t.group }

// Update replaces all pixels without changing the texture or its bind group.
// Image dimensions must match the original upload. Automatic mip chains are
// regenerated, on the CPU for an uploaded texture and on the GPU for a
// RenderTarget; caller-supplied multi-level chains require UpdateLevels.
func (t *Texture) Update(src image.Image) error {
	if t.r.closed.Load() {
		return ErrClosed
	}
	if t.format == gputypes.TextureFormatRGBA16Float {
		return &Error{Op: "update texture", Err: errors.New("a Float16 texture cannot be updated; draw it with Frame.RenderTo")}
	}
	if t.mipLevels > 1 && !t.autoMips {
		return &Error{Op: "update texture", Err: errors.New("caller-supplied mip chains require UpdateLevels")}
	}
	w, h, err := imageSize("update texture", src)
	if err != nil {
		return err
	}
	if w != t.width || h != t.height {
		return &Error{Op: "update texture", Err: fmt.Errorf("size %dx%d differs from %dx%d", w, h, t.width, t.height)}
	}
	gpuMips := t.generatesMips()
	pixels, err := uploadPixels(src, t.autoMips && !gpuMips, t.mipOptions)
	if err != nil {
		return err
	}
	return t.updatePackedLevels(pixels, gpuMips)
}

// ReadPixels returns a fresh, tightly packed NRGBA copy of this texture: the
// stored bytes, which for an SRGB texture are still sRGB-encoded. A Float16
// texture is clamped to [0, 1] and rounded to 8 bits. It synchronizes with
// pending GPU work; use it only when a CPU consumer needs the pixels.
// Sampling this texture in a render pass does not require readback.
func (t *Texture) ReadPixels(ctx context.Context) (*image.NRGBA, error) {
	if t.r.closed.Load() {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var img *image.NRGBA
	var err error
	if runErr := t.r.exec.run(ctx, func() {
		if t.closed || t.r.device == nil {
			err = ErrClosed
			return
		}
		if err = ctx.Err(); err != nil {
			return
		}
		img, err = t.readPixels(ctx)
	}); runErr != nil {
		return nil, runErr
	}
	return img, err
}

func (t *Texture) readPixels(ctx context.Context) (*image.NRGBA, error) {
	r := t.r
	if !t.written {
		return nil, &Error{Op: "read texture", Err: errors.New("nothing has been written to or drawn into the texture")}
	}
	if err := r.checkReadbackSize("read texture", t.width, t.height); err != nil {
		return nil, err
	}
	stride := alignedBytesPerRow(t.width)
	staging, err := r.device.CreateBuffer(&wgpu.BufferDescriptor{
		Label: r.label + ":texture-readback", Size: uint64(stride * t.height),
		Usage: wgpu.BufferUsageCopyDst | wgpu.BufferUsageMapRead,
	})
	if err != nil {
		return nil, &Error{Op: "create texture readback buffer", Err: err}
	}
	defer staging.Release()
	enc, err := r.device.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: r.label + ":texture-readback"})
	if err != nil {
		return nil, &Error{Op: "create encoder", Err: err}
	}
	if t.drawn {
		return t.readByBlit(ctx, enc, staging, stride, t.group, t.width, t.height)
	}
	// A sampled texture rests in the TextureBinding state: gogpu leaves it
	// there after every write and does not track sampling. The copy needs
	// CopySrc, and later sampling needs the texture put back.
	transition(enc, t.texture, gputypes.TextureUsageTextureBinding, gputypes.TextureUsageCopySrc)
	encodeReadback(enc, t.texture, staging, t.width, t.height, stride)
	transition(enc, t.texture, gputypes.TextureUsageCopySrc, gputypes.TextureUsageTextureBinding)
	cmd, err := r.submit(enc)
	if err != nil {
		return nil, err
	}
	img, _, _, err := mapImage(ctx, r.device, staging, t.width, t.height, stride)
	if err != nil {
		return nil, err
	}
	r.device.FreeCommandBuffer(cmd)
	return img, nil
}

// readByBlit reads back w×h texels of the level that group binds, from a
// texture that has been drawn into; level 0 of the full view by default. Once a
// texture has been a render attachment, gogpu's usage tracker records that
// usage and, before any later submission that copies from the texture,
// records its own barrier from the attachment layout. On Vulkan the texture
// is in the sampled layout by then, so that barrier and every one after it
// name the wrong state, and nothing recorded in the command buffer runs
// before it. Sampling is not tracked, so the texture is instead drawn into
// a scratch target that has no TextureBinding usage, and the scratch target
// is copied out exactly as the main target is. The scratch target is new,
// so its first pass starts from the undefined layout. Rendering a same-size
// quad loads level 0 by integer pixel coordinates, independent of the
// sampling filter, mip selection and wrapping mode.
//
// Sampling an SRGB texture decodes it, so its scratch target is also sRGB
// and encodes the texels back on write. A view of the texture as
// RGBA8Unorm would avoid the round trip, but gogpu v0.34.5 honours view
// formats only on Vulkan: Metal never sets PixelFormatView and DX12
// creates the texture with a fully typed format. A Float16 texture is
// converted to RGBA8Unorm by the write.
func (t *Texture) readByBlit(ctx context.Context, enc *wgpu.CommandEncoder, staging *wgpu.Buffer, stride int, group *wgpu.BindGroup, w, h int) (_ *image.NRGBA, err error) {
	r := t.r
	format := gputypes.TextureFormatRGBA8Unorm
	if t.format == gputypes.TextureFormatRGBA8UnormSrgb {
		format = t.format
	}
	blit, err := r.blitPipeline(format)
	if err != nil {
		enc.DiscardEncoding()
		return nil, err
	}
	scratch, err := r.device.CreateTexture(&wgpu.TextureDescriptor{
		Label:         r.label + ":texture-readback-scratch",
		Size:          wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
		MipLevelCount: 1,
		SampleCount:   1,
		Dimension:     gputypes.TextureDimension2D,
		Format:        format,
		Usage:         gputypes.TextureUsageRenderAttachment | gputypes.TextureUsageCopySrc,
	})
	if err != nil {
		enc.DiscardEncoding()
		return nil, &Error{Op: "create texture readback scratch", Err: err}
	}
	defer scratch.Release()
	view, err := r.device.CreateTextureView(scratch, nil)
	if err != nil {
		enc.DiscardEncoding()
		return nil, &Error{Op: "create texture readback scratch view", Err: err}
	}
	defer view.Release()
	pass, err := enc.BeginRenderPass(&wgpu.RenderPassDescriptor{
		Label:            r.label + ":texture-readback-blit",
		ColorAttachments: []wgpu.RenderPassColorAttachment{{View: view, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore}},
	})
	if err != nil {
		enc.DiscardEncoding()
		return nil, &Error{Op: "begin texture readback blit", Err: err}
	}
	pass.SetPipeline(blit)
	pass.SetBindGroup(0, group, nil)
	pass.Draw(gputypes.DrawArgs{VertexCount: 6, InstanceCount: 1})
	if err := pass.End(); err != nil {
		enc.DiscardEncoding()
		return nil, &Error{Op: "end texture readback blit", Err: err}
	}
	transition(enc, scratch, gputypes.TextureUsageRenderAttachment, gputypes.TextureUsageCopySrc)
	encodeReadback(enc, scratch, staging, w, h, stride)
	cmd, err := r.submit(enc)
	if err != nil {
		return nil, err
	}
	img, _, _, err := mapImage(ctx, r.device, staging, w, h, stride)
	if err != nil {
		return nil, err
	}
	r.device.FreeCommandBuffer(cmd)
	return img, nil
}

// blitShader loads level 0 over the whole target, ignoring sampler state.
// Its UVs remain explicit because naga v0.19.0 miscompiles the derived form for
// Metal; see TestDerivedUVShader.
const blitShader = `
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
@fragment fn fs_main(o: Out) -> @location(0) vec4<f32> { return textureLoad(tex, vec2<i32>(o.pos.xy), 0); }`

// blitPipeline returns the readback blit pipeline that writes format,
// creating it on first use. It runs on the executor; the pipelines, their
// layout and their module are released with the device.
func (r *Renderer) blitPipeline(format gputypes.TextureFormat) (*wgpu.RenderPipeline, error) {
	if p := r.blit[format]; p != nil {
		return p, nil
	}
	if r.sampledLayout == nil {
		return nil, &Error{Op: "create readback blit", Err: errors.New("no sampled texture layout")}
	}
	// The layout and module stay alive with the pipelines: the Vulkan
	// backend binds descriptor sets against the layout at draw time.
	if r.blitModule == nil {
		module, err := r.device.CreateShaderModule(&wgpu.ShaderModuleDescriptor{Label: r.label + ":readback-blit", WGSL: blitShader})
		if err != nil {
			return nil, &Error{Op: "create readback blit shader", Err: err}
		}
		r.blitModule = module
	}
	layout, err := r.sampledPipelineLayout()
	if err != nil {
		return nil, err
	}
	pipeline, err := r.device.CreateRenderPipeline(&wgpu.RenderPipelineDescriptor{
		Label:       r.label + ":readback-blit",
		Layout:      layout,
		Vertex:      wgpu.VertexState{Module: r.blitModule, EntryPoint: "vs_main"},
		Fragment:    &wgpu.FragmentState{Module: r.blitModule, EntryPoint: "fs_main", Targets: []gputypes.ColorTargetState{{Format: format, WriteMask: gputypes.ColorWriteMaskAll}}},
		Primitive:   gputypes.PrimitiveState{Topology: gputypes.PrimitiveTopologyTriangleList, CullMode: gputypes.CullModeNone},
		Multisample: gputypes.DefaultMultisampleState(),
	})
	if err != nil {
		return nil, &Error{Op: "create readback blit pipeline", Err: err}
	}
	if r.blit == nil {
		r.blit = make(map[gputypes.TextureFormat]*wgpu.RenderPipeline)
	}
	r.blit[format] = pipeline
	return pipeline, nil
}

// Close releases this texture's GPU resources. It is safe to call more than
// once; Renderer.Close also releases any textures left open.
func (t *Texture) Close() error {
	if t.r.closed.Load() {
		return nil
	}
	return t.r.exec.run(context.Background(), func() {
		if !t.closed {
			t.release()
			delete(t.r.textures, t)
		}
	})
}

func (t *Texture) release() {
	if t.closed {
		return
	}
	t.releaseMipTargets()
	if t.group != nil {
		t.group.Release()
	}
	if t.sampler != nil {
		t.sampler.Release()
	}
	if t.view != nil {
		t.view.Release()
	}
	if t.texture != nil {
		t.texture.Release()
	}
	if t.depthView != nil {
		t.depthView.Release()
	}
	if t.depthTex != nil {
		t.depthTex.Release()
	}
	t.group, t.sampler, t.view, t.colorView, t.texture, t.depthView, t.depthTex = nil, nil, nil, nil, nil, nil, nil
	t.closed = true
}
