package ntgpuimage

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync/atomic"
	"time"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

// Options configures a Renderer at construction.
type Options struct {
	Width, Height int    // initial target size in pixels; both must be > 0
	Depth         bool   // allocate a Depth24Plus attachment alongside the color target
	Label         string // optional prefix for GPU object labels

	// Backends restricts native backend selection. Zero means all.
	Backends gputypes.Backends
	// PowerPreference is forwarded to RequestAdapter.
	PowerPreference gputypes.PowerPreference
	// ForceFallback requests the software (CPU) adapter. Useful for
	// deterministic tests; ignored where no such adapter exists.
	ForceFallback bool
}

// Frame is handed to the draw callback of Render. Record render passes
// into Encoder against Color (and Depth when allocated); the Renderer
// appends the readback copy and submits Encoder once the callback returns.
// Every field is valid only until the callback returns.
type Frame struct {
	Device  *wgpu.Device // the renderer's device, for queue writes and resource updates
	Encoder *wgpu.CommandEncoder
	Color   *wgpu.TextureView // RGBA8Unorm, RenderAttachment|CopySrc; inside RenderTo, the texture's Format
	Depth   *wgpu.TextureView // nil unless Options.Depth
	Width   int
	Height  int
	Seq     uint64 // frame sequence number, starting at 1

	r      *Renderer
	nested bool  // set on the Frame handed to a RenderTo callback
	err    error // a RenderTo failure the draw callback did not return
}

// RenderTo runs draw against t instead of the main target. The Frame passed
// to draw shares this frame's Device, Encoder and Seq, and its Color, Depth,
// Width and Height describe t. Passes recorded in draw run before passes
// recorded afterwards, so the rest of this frame, and any later frame, can
// sample t. t must have been created with TextureOptions.RenderTarget.
//
// For a texture created with Mipmaps, RenderTo then regenerates every lower
// level from level 0 on the GPU, inside the same frame; see
// mipgen_target.go for the filter. Such a texture can be drawn into once
// per frame. RenderTo cannot be nested, and the nested Frame is cleared
// when draw returns. An error from draw fails the whole frame even if the caller does
// not return it, because the texture pass may be half recorded. Errors
// reported before draw runs leave the frame usable.
func (f *Frame) RenderTo(t *Texture, draw func(*Frame) error) error {
	const op = "render to texture"
	if f.nested {
		return &Error{Op: op, Err: errors.New("RenderTo cannot be nested")}
	}
	if t == nil {
		return &Error{Op: op, Err: errors.New("nil texture")}
	}
	if t.r != f.r {
		return &Error{Op: op, Err: errors.New("texture belongs to another renderer")}
	}
	if t.closed || t.texture == nil {
		return ErrClosed
	}
	if !t.renderTarget {
		return &Error{Op: op, Err: errors.New("texture was not created with RenderTarget")}
	}
	if t.generatesMips() && t.mipFrame == f.Seq {
		return &Error{Op: op, Err: errors.New("a mipmapped render target can be drawn into once per frame")}
	}
	// On Vulkan a pass leaves an attachment that also has TextureBinding
	// usage in the GENERAL layout, and a pass with LoadOp Load expects to
	// find it there, while sampling needs SHADER_READ_ONLY. gogpu's usage
	// tracker records only the attachment usage, so it never moves the
	// texture itself. A written texture rests in SHADER_READ_ONLY and is
	// moved to GENERAL before the pass; a new one starts undefined and the
	// pass handles that. Both are no-ops on Metal and in the browser.
	if t.written {
		transition(f.Encoder, t.texture, gputypes.TextureUsageTextureBinding, generalLayout)
	}
	sub := &Frame{Device: f.Device, Encoder: f.Encoder, Color: t.colorView, Depth: t.depthView, Width: t.width, Height: t.height, Seq: f.Seq, r: f.r, nested: true}
	err := draw(sub)
	*sub = Frame{}
	if err == nil {
		transition(f.Encoder, t.texture, generalLayout, gputypes.TextureUsageTextureBinding)
		if t.generatesMips() {
			// Level 0 is in the sampled layout now; fill the rest of the
			// chain before anything later in this frame samples it.
			err = t.generateMips(f.Encoder)
		}
	}
	if err != nil {
		// The pass may be half recorded, so the frame cannot be submitted
		// even if the caller ignores this error.
		err = &Error{Op: op, Err: err}
		if f.err == nil {
			f.err = err
		}
		return err
	}
	t.written, t.drawn, t.mipFrame = true, true, f.Seq
	return nil
}

// generalLayout names the Vulkan GENERAL image layout in a barrier. gogpu
// derives the layout from a usage, and StorageBinding is the only usage it
// maps to GENERAL; TextureBinding is added so the access and stage masks
// also cover fragment sampling. The texture need not have been created
// with either usage: the backend uses the mask only to pick layouts and
// masks, and Metal and the browser ignore barriers altogether.
const generalLayout = gputypes.TextureUsageStorageBinding | gputypes.TextureUsageTextureBinding

// Stats reports where one Render spent its time.
type Stats struct {
	Seq         uint64
	Draw        time.Duration // the caller's draw callback
	Submit      time.Duration // copy encode + queue submit
	Map         time.Duration // staging-buffer map wait (GPU sync)
	Copy        time.Duration // de-pad into the NRGBA image
	Total       time.Duration
	BytesPerRow int // padded staging row stride
}

// Renderer owns a GPU device and an offscreen render target, and reads
// frames back as *image.NRGBA. All GPU work runs on an internal executor
// goroutine pinned to one OS thread. Renderer is safe for concurrent use;
// calls are serialized. Its methods must not be called from inside Do or a
// draw callback, which already occupy the executor.
type Renderer struct {
	exec *executor

	instance    *wgpu.Instance
	adapter     *wgpu.Adapter
	device      *wgpu.Device
	adapterInfo gputypes.AdapterInfo
	limits      gputypes.Limits // immutable after New, so readable off the executor
	label       string
	depth       bool

	target        target
	sampledLayout *wgpu.BindGroupLayout
	blit          map[gputypes.TextureFormat]*wgpu.RenderPipeline // readback of drawn textures by scratch format, created on first use
	blitLayout    *wgpu.PipelineLayout                            // sampled-image-only layout, shared with mip generation
	blitModule    *wgpu.ShaderModule
	mips          map[gputypes.TextureFormat]*wgpu.RenderPipeline // render-target mip generation by format, created on first use
	mipModule     *wgpu.ShaderModule
	mipSampler    *wgpu.Sampler
	mipSink       *wgpu.Buffer // one padded row; see mipgen_target.go
	textures      map[*Texture]struct{}

	seq    uint64
	closed atomic.Bool
}

// New creates the instance, adapter, device, and an initial target of
// opts.Width×opts.Height. Everything created so far is released on failure.
func New(ctx context.Context, opts Options) (*Renderer, error) {
	if opts.Width <= 0 || opts.Height <= 0 {
		return nil, &Error{Op: "new", Err: fmt.Errorf("invalid size %dx%d", opts.Width, opts.Height)}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &Renderer{exec: newExecutor(), label: opts.Label, depth: opts.Depth}
	var err error
	_ = r.exec.run(context.Background(), func() { err = r.init(opts) })
	if err != nil {
		_ = r.exec.run(context.Background(), r.releaseAll)
		r.exec.stop()
		return nil, err
	}
	return r, nil
}

func (r *Renderer) init(opts Options) error {
	backends := opts.Backends
	if backends == 0 {
		backends = gputypes.BackendsAll
	}
	inst, err := wgpu.CreateInstance(&wgpu.InstanceDescriptor{Backends: backends})
	if err != nil {
		return &Error{Op: "create instance", Err: err}
	}
	r.instance = inst
	adapter, err := inst.RequestAdapter(&wgpu.RequestAdapterOptions{
		PowerPreference:      opts.PowerPreference,
		ForceFallbackAdapter: opts.ForceFallback,
	})
	if err != nil || adapter == nil {
		return adapterError(err)
	}
	r.adapter = adapter
	r.adapterInfo = adapter.Info()
	dev, err := adapter.RequestDevice(nil)
	if err != nil {
		return &Error{Op: "request device", Err: err}
	}
	r.device = dev
	r.limits = dev.Limits()
	return r.allocate(opts.Width, opts.Height)
}

// adapterError wraps a RequestAdapter failure so callers can test for the
// documented ErrNoAdapter sentinel with errors.Is while keeping gogpu's
// message. gogpu reports a missing adapter as an error on both backends;
// a nil adapter with a nil error is treated the same way.
func adapterError(cause error) error {
	if cause == nil {
		return &Error{Op: "request adapter", Err: ErrNoAdapter}
	}
	return &Error{Op: "request adapter", Err: fmt.Errorf("%w: %w", ErrNoAdapter, cause)}
}

// target groups the size-dependent GPU objects so allocate can build a new
// set before releasing the old one (a failed Resize keeps the old target).
type target struct {
	width, height int
	bytesPerRow   int
	color         *wgpu.Texture
	colorView     *wgpu.TextureView
	depthTex      *wgpu.Texture
	depthView     *wgpu.TextureView
	staging       *wgpu.Buffer

	drawn bool // a frame was submitted to this target
}

func (t *target) release() {
	if t.staging != nil {
		t.staging.Release()
	}
	if t.depthView != nil {
		t.depthView.Release()
	}
	if t.depthTex != nil {
		t.depthTex.Release()
	}
	if t.colorView != nil {
		t.colorView.Release()
	}
	if t.color != nil {
		t.color.Release()
	}
	*t = target{}
}

// checkTextureSize and checkReadbackSize reject dimensions the device cannot
// allocate. Native backends also validate at creation, but the browser
// reports creation failures asynchronously, so without these checks a failed
// Resize there would replace a working target with an invalid one.
func (r *Renderer) checkTextureSize(op string, w, h int) error {
	if limit := int(r.limits.MaxTextureDimension2D); limit > 0 && (w > limit || h > limit) {
		return &Error{Op: op, Err: fmt.Errorf("size %dx%d exceeds the device texture limit %d", w, h, limit)}
	}
	return nil
}

func (r *Renderer) checkReadbackSize(op string, w, h int) error {
	size, limit := uint64(alignedBytesPerRow(w))*uint64(h), r.limits.MaxBufferSize
	if limit > 0 && size > limit {
		return &Error{Op: op, Err: fmt.Errorf("size %dx%d needs a %d-byte readback buffer, above the device limit %d", w, h, size, limit)}
	}
	return nil
}

func (r *Renderer) buildTarget(w, h int) (_ target, err error) {
	if err = r.checkTextureSize("create target", w, h); err != nil {
		return target{}, err
	}
	if err = r.checkReadbackSize("create target", w, h); err != nil {
		return target{}, err
	}
	t := target{width: w, height: h, bytesPerRow: alignedBytesPerRow(w)}
	defer func() {
		if err != nil {
			t.release()
		}
	}()
	size := wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1}
	t.color, err = r.device.CreateTexture(&wgpu.TextureDescriptor{
		Label:         r.label + ":color",
		Size:          size,
		MipLevelCount: 1,
		SampleCount:   1,
		Dimension:     gputypes.TextureDimension2D,
		Format:        gputypes.TextureFormatRGBA8Unorm,
		Usage:         gputypes.TextureUsageRenderAttachment | gputypes.TextureUsageCopySrc,
	})
	if err != nil {
		return target{}, &Error{Op: "create color texture", Err: err}
	}
	t.colorView, err = r.device.CreateTextureView(t.color, nil)
	if err != nil {
		return target{}, &Error{Op: "create color view", Err: err}
	}
	if r.depth {
		t.depthTex, err = r.device.CreateTexture(&wgpu.TextureDescriptor{
			Label:         r.label + ":depth",
			Size:          size,
			MipLevelCount: 1,
			SampleCount:   1,
			Dimension:     gputypes.TextureDimension2D,
			Format:        gputypes.TextureFormatDepth24Plus,
			Usage:         gputypes.TextureUsageRenderAttachment,
		})
		if err != nil {
			return target{}, &Error{Op: "create depth texture", Err: err}
		}
		t.depthView, err = r.device.CreateTextureView(t.depthTex, nil)
		if err != nil {
			return target{}, &Error{Op: "create depth view", Err: err}
		}
	}
	if t.staging, err = r.newStaging(&t); err != nil {
		return target{}, err
	}
	return t, nil
}

func (r *Renderer) newStaging(t *target) (*wgpu.Buffer, error) {
	staging, err := r.device.CreateBuffer(&wgpu.BufferDescriptor{
		Label: r.label + ":staging",
		Size:  uint64(t.bytesPerRow * t.height),
		Usage: wgpu.BufferUsageCopyDst | wgpu.BufferUsageMapRead,
	})
	if err != nil {
		return nil, &Error{Op: "create staging buffer", Err: err}
	}
	return staging, nil
}

// allocate builds a new target and only then releases the old one, so a
// failure leaves the Renderer usable at its previous size.
func (r *Renderer) allocate(w, h int) error {
	t, err := r.buildTarget(w, h)
	if err != nil {
		return err
	}
	r.target.release()
	r.target = t
	return nil
}

func (r *Renderer) releaseAll() {
	for t := range r.textures {
		t.release()
	}
	clear(r.textures)
	for _, p := range r.blit {
		p.Release()
	}
	clear(r.blit)
	for _, p := range r.mips {
		p.Release()
	}
	clear(r.mips)
	if r.mipModule != nil {
		r.mipModule.Release()
		r.mipModule = nil
	}
	if r.mipSampler != nil {
		r.mipSampler.Release()
		r.mipSampler = nil
	}
	if r.mipSink != nil {
		r.mipSink.Release()
		r.mipSink = nil
	}
	if r.blitLayout != nil {
		r.blitLayout.Release()
		r.blitLayout = nil
	}
	if r.blitModule != nil {
		r.blitModule.Release()
		r.blitModule = nil
	}
	if r.sampledLayout != nil {
		r.sampledLayout.Release()
		r.sampledLayout = nil
	}
	r.target.release()
	if r.device != nil {
		r.device.Release()
		r.device = nil
	}
	if r.adapter != nil {
		r.adapter.Release()
		r.adapter = nil
	}
	if r.instance != nil {
		r.instance.Release()
		r.instance = nil
	}
}

// Do runs f on the executor thread with the device. Use it for pipeline,
// shader, and buffer setup so those calls obey the single-thread rule.
func (r *Renderer) Do(f func(dev *wgpu.Device) error) error {
	if r.closed.Load() {
		return ErrClosed
	}
	var err error
	if runErr := r.exec.run(context.Background(), func() {
		if r.device == nil {
			err = ErrClosed
			return
		}
		err = f(r.device)
	}); runErr != nil {
		return runErr
	}
	return err
}

// AdapterInfo reports immutable adapter metadata: its name, backend, and
// device type. A DeviceTypeCPU adapter is wgpu's software rasterizer.
func (r *Renderer) AdapterInfo() gputypes.AdapterInfo { return r.adapterInfo }

// Size reports the current target size in pixels.
func (r *Renderer) Size() (w, h int) {
	_ = r.exec.run(context.Background(), func() { w, h = r.target.width, r.target.height })
	return w, h
}

// Close releases every GPU object and stops the executor. Idempotent.
func (r *Renderer) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	_ = r.exec.run(context.Background(), r.releaseAll)
	r.exec.stop()
	return nil
}

// Resize replaces the color/depth textures and the staging buffer. It is a
// no-op when the size is unchanged, and on failure the old target is kept.
func (r *Renderer) Resize(w, h int) error {
	if r.closed.Load() {
		return ErrClosed
	}
	if w <= 0 || h <= 0 {
		return &Error{Op: "resize", Err: fmt.Errorf("invalid size %dx%d", w, h)}
	}
	var err error
	if runErr := r.exec.run(context.Background(), func() {
		if r.device == nil {
			err = ErrClosed
			return
		}
		if w == r.target.width && h == r.target.height {
			return
		}
		err = r.allocate(w, h)
	}); runErr != nil {
		return runErr
	}
	return err
}

// Render runs draw against a fresh command encoder, appends the readback
// copy, submits, maps the staging buffer, and returns a new NRGBA image
// that the caller owns. One frame is in flight at a time.
//
// Render returns ctx.Err() if ctx ends while the call waits for the GPU
// thread, during draw, or while the GPU finishes the frame. It does not
// interrupt a running draw callback. After a cancellation the GPU may still
// complete the abandoned frame; later calls are unaffected. Watching a
// context that can end adds about a tenth of a millisecond to a native
// readback.
func (r *Renderer) Render(ctx context.Context, draw func(*Frame) error) (*image.NRGBA, Stats, error) {
	if draw == nil {
		return nil, Stats{}, &Error{Op: "draw", Err: fmt.Errorf("nil draw callback")}
	}
	return r.execute(ctx, draw, true)
}

// RenderToTexture draws and submits without copying pixels to Go, for
// callers that read back only some frames. The color attachment keeps the
// frame until the next draw or Resize, and ReadPixels copies it on request.
// The attachment cannot be sampled: it has no TextureBinding usage.
//
// Native submission waits for the GPU when ctx cannot end, such as
// context.Background. Otherwise, and always in the browser, it returns once
// the frame is submitted.
func (r *Renderer) RenderToTexture(ctx context.Context, draw func(*Frame) error) (Stats, error) {
	if draw == nil {
		return Stats{}, &Error{Op: "draw", Err: fmt.Errorf("nil draw callback")}
	}
	_, st, err := r.execute(ctx, draw, false)
	return st, err
}

// ReadPixels explicitly copies and maps the current color attachment into a
// caller-owned Go image. Call after drawing; Render combines both steps in one
// submission. Concurrent callers read the most recently submitted frame. It
// fails if nothing was drawn since New or the last Resize.
func (r *Renderer) ReadPixels(ctx context.Context) (*image.NRGBA, Stats, error) {
	return r.execute(ctx, nil, true)
}

func (r *Renderer) execute(ctx context.Context, draw func(*Frame) error, readback bool) (*image.NRGBA, Stats, error) {
	if r.closed.Load() {
		return nil, Stats{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, Stats{}, err
	}
	var (
		img *image.NRGBA
		st  Stats
		err error
	)
	if runErr := r.exec.run(ctx, func() {
		if r.device == nil {
			err = ErrClosed
			return
		}
		if err = ctx.Err(); err != nil {
			return
		}
		img, st, err = r.render(ctx, draw, readback)
	}); runErr != nil {
		return nil, Stats{}, runErr
	}
	return img, st, err
}

func (r *Renderer) render(ctx context.Context, draw func(*Frame) error, readback bool) (*image.NRGBA, Stats, error) {
	start := time.Now()
	if draw != nil {
		r.seq++
	}
	t := &r.target
	st := Stats{Seq: r.seq, BytesPerRow: t.bytesPerRow}

	if draw == nil && !t.drawn {
		return nil, st, &Error{Op: "read pixels", Err: fmt.Errorf("no frame drawn since the target was created")}
	}
	enc, err := r.device.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: r.label + ":frame"})
	if err != nil {
		return nil, st, &Error{Op: "create encoder", Err: err}
	}
	// gogpu records one usage per texture for each submission, and before
	// the submission it moves the texture there from its previous usage.
	// That covers a draw alone and a readback alone. A draw followed by a
	// readback is recorded as CopySrc, so the texture arrives in CopySrc and
	// both transitions around the draw are declared here. They are layout
	// barriers on Vulkan and no-ops on Metal and in the browser. A new
	// texture has no previous usage and is not moved.
	drawAndCopy := draw != nil && readback
	if drawAndCopy && t.drawn {
		transition(enc, t.color, gputypes.TextureUsageCopySrc, gputypes.TextureUsageRenderAttachment)
	} else if drawAndCopy {
		// A new target is undefined. Moving it to the attachment layout
		// here keeps the transition after the draw correct even when the
		// draw only renders into textures and never attaches the target.
		transition(enc, t.color, 0, gputypes.TextureUsageRenderAttachment)
	}
	if draw != nil {
		frame := &Frame{Device: r.device, Encoder: enc, Color: t.colorView, Depth: t.depthView, Width: t.width, Height: t.height, Seq: r.seq, r: r}
		err := draw(frame)
		if err == nil {
			err = frame.err
		}
		if err != nil {
			// Abandon the encoder explicitly. A dropped encoder keeps its Metal
			// command buffer uncommitted. Metal's queue allows 64 of those by
			// default, and the next CreateCommandEncoder then blocks forever,
			// wedging the executor.
			enc.DiscardEncoding()
			return nil, st, &Error{Op: "draw", Err: err}
		}
		if err := ctx.Err(); err != nil {
			enc.DiscardEncoding()
			return nil, st, err
		}
	}
	st.Draw = time.Since(start)

	submitStart := time.Now()
	if drawAndCopy {
		// Without this gogpu also rejects the copy, because the encoder has
		// used the texture as an attachment.
		transition(enc, t.color, gputypes.TextureUsageRenderAttachment, gputypes.TextureUsageCopySrc)
	}
	if readback {
		if t.staging == nil { // dropped after a failed or cancelled wait
			if t.staging, err = r.newStaging(t); err != nil {
				enc.DiscardEncoding()
				return nil, st, err
			}
		}
		encodeReadback(enc, t.color, t.staging, t.width, t.height, t.bytesPerRow)
	}
	cmd, err := r.submit(enc)
	if err != nil {
		return nil, st, err
	}
	t.drawn = true
	st.Submit = time.Since(submitStart)
	if !readback {
		// Waiting for the GPU cannot be interrupted, so only a context that
		// cannot end waits here.
		if ctx.Done() == nil {
			r.device.Poll(wgpu.PollWait)
			r.device.FreeCommandBuffer(cmd)
		}
		st.Total = time.Since(start)
		return nil, st, nil
	}

	img, mapped, copied, err := mapImage(ctx, r.device, t.staging, t.width, t.height, t.bytesPerRow)
	if err != nil {
		// Do not map this buffer again. gogpu keeps the abandoned request
		// registered against its submission and resolves it together with
		// the next request on the same buffer, which then reports a
		// cancelled map. The next readback allocates a new buffer.
		t.staging.Release()
		t.staging = nil
		return nil, st, err
	}
	r.device.FreeCommandBuffer(cmd)
	st.Map, st.Copy = mapped, copied
	st.Total = time.Since(start)
	return img, st, nil
}

// submit finishes enc and queues it. The caller frees the returned command
// buffer only once the GPU has finished it, which a resolved map or a
// blocking poll establishes. After a cancelled or failed wait the buffer is
// left to gogpu, which recycles submitted encoders on completion.
func (r *Renderer) submit(enc *wgpu.CommandEncoder) (*wgpu.CommandBuffer, error) {
	cmd, err := enc.Finish() // a failed Finish releases the encoder itself
	if err != nil {
		return nil, &Error{Op: "finish", Err: err}
	}
	if _, err := r.device.Queue().Submit(cmd); err != nil {
		cmd.Release()
		return nil, &Error{Op: "submit", Err: err}
	}
	return cmd, nil
}
