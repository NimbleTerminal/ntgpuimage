package ntgpuimage

import (
	"context"
	"fmt"
	"image"
	"image/draw"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

// Texture is an RGBA8 image owned by a Renderer. It can be sampled in a draw
// callback, updated without reallocating, or copied back to Go pixels.
// Binding 0 is texture_2d<f32>; binding 1 is a filtering sampler. UVs clamp
// at the edges.
type Texture struct {
	r             *Renderer
	texture       *wgpu.Texture
	view          *wgpu.TextureView
	sampler       *wgpu.Sampler
	group         *wgpu.BindGroup
	width, height int
	closed        bool // accessed only on the renderer executor
}

// TextureOptions controls the sampling filter. Zero selects linear filtering.
// Nearest filtering is useful for pixel art or an atlas without padded tiles.
type TextureOptions struct {
	Filter gputypes.FilterMode
}

// UploadTexture copies an image into a sampled GPU texture. The source image
// can have any bounds or image.Image implementation; pixels are copied before
// this method returns. Close the Texture when it is no longer needed, or let
// Renderer.Close release it with the device.
func (r *Renderer) UploadTexture(src image.Image, opts TextureOptions) (*Texture, error) {
	if r.closed.Load() {
		return nil, ErrClosed
	}
	filter := opts.Filter
	if filter == 0 {
		filter = gputypes.FilterModeLinear
	}
	if filter != gputypes.FilterModeLinear && filter != gputypes.FilterModeNearest {
		return nil, &Error{Op: "upload texture", Err: fmt.Errorf("invalid filter %v", filter)}
	}
	// Reject an unusable image from its bounds, before copying its pixels.
	w, h, err := imageSize("upload texture", src)
	if err != nil {
		return nil, err
	}
	if err := r.checkTextureSize("upload texture", w, h); err != nil {
		return nil, err
	}
	pixels := packPixels(src)
	var result *Texture
	if runErr := r.exec.run(context.Background(), func() {
		if r.device == nil {
			err = ErrClosed
			return
		}
		result, err = r.uploadTexture(pixels, w, h, filter)
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

func (r *Renderer) uploadTexture(pixels []byte, w, h int, filter gputypes.FilterMode) (_ *Texture, err error) {
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
	t := &Texture{r: r, width: w, height: h}
	defer func() {
		if err != nil {
			t.release()
		}
	}()
	t.texture, err = r.device.CreateTexture(&wgpu.TextureDescriptor{
		Label:         r.label + ":sampled-image",
		Size:          wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
		MipLevelCount: 1,
		SampleCount:   1,
		Dimension:     gputypes.TextureDimension2D,
		Format:        gputypes.TextureFormatRGBA8Unorm,
		Usage:         gputypes.TextureUsageTextureBinding | gputypes.TextureUsageCopyDst | gputypes.TextureUsageCopySrc,
	})
	if err != nil {
		return nil, &Error{Op: "create sampled texture", Err: err}
	}
	t.view, err = r.device.CreateTextureView(t.texture, nil)
	if err != nil {
		return nil, &Error{Op: "create sampled texture view", Err: err}
	}
	t.sampler, err = r.device.CreateSampler(&wgpu.SamplerDescriptor{
		Label: r.label + ":linear-sampler", AddressModeU: gputypes.AddressModeClampToEdge, AddressModeV: gputypes.AddressModeClampToEdge,
		MagFilter: filter, MinFilter: filter, MipmapFilter: gputypes.FilterModeNearest,
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
	if err = t.write(pixels); err != nil {
		return nil, err
	}
	if r.textures == nil {
		r.textures = make(map[*Texture]struct{})
	}
	r.textures[t] = struct{}{}
	return t, nil
}

func (t *Texture) write(pixels []byte) error {
	if err := t.r.device.Queue().WriteTexture(
		&wgpu.ImageCopyTexture{Texture: t.texture}, pixels,
		&wgpu.ImageDataLayout{BytesPerRow: uint32(t.width * 4), RowsPerImage: uint32(t.height)},
		&wgpu.Extent3D{Width: uint32(t.width), Height: uint32(t.height), DepthOrArrayLayers: 1},
	); err != nil {
		return &Error{Op: "write sampled texture", Err: err}
	}
	return nil
}

// Size returns the immutable texture dimensions in pixels.
func (t *Texture) Size() (int, int) { return t.width, t.height }

// BindGroupLayout is the layout to include in a caller's pipeline layout.
// Use BindGroup at the matching group index inside Renderer.Render/Do.
// These raw GPU handles are valid only while the Texture and Renderer are open.
func (t *Texture) BindGroupLayout() *wgpu.BindGroupLayout { return t.r.sampledLayout }
func (t *Texture) BindGroup() *wgpu.BindGroup             { return t.group }

// Update replaces all pixels without changing the texture or its bind group.
// Image dimensions must match the original upload.
func (t *Texture) Update(src image.Image) error {
	if t.r.closed.Load() {
		return ErrClosed
	}
	w, h, err := imageSize("update texture", src)
	if err != nil {
		return err
	}
	if w != t.width || h != t.height {
		return &Error{Op: "update texture", Err: fmt.Errorf("size %dx%d differs from %dx%d", w, h, t.width, t.height)}
	}
	pixels := packPixels(src)
	if runErr := t.r.exec.run(context.Background(), func() {
		if t.closed || t.r.device == nil {
			err = ErrClosed
			return
		}
		err = t.write(pixels)
	}); runErr != nil {
		return runErr
	}
	return err
}

// ReadPixels returns a fresh, tightly packed NRGBA copy of this texture.
// It synchronizes with pending GPU work; use it only when a CPU consumer needs
// the pixels. Sampling this texture in a render pass does not require readback.
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
	t.group, t.sampler, t.view, t.texture = nil, nil, nil, nil
	t.closed = true
}
