package gpuimage

import (
	"context"
	"errors"
	"image"
	"os"
	"testing"
	"time"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

// newTestRenderer skips unless GPUIMAGE_GPU_TEST=1, because CI hosts have
// no GPU and the software backend is not guaranteed on every platform.
func newTestRenderer(t *testing.T, opts Options) *Renderer {
	t.Helper()
	if os.Getenv("GPUIMAGE_GPU_TEST") != "1" {
		t.Skip("set GPUIMAGE_GPU_TEST=1 to run GPU tests")
	}
	r, err := New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// clearTo returns a draw callback that only clears the color target.
func clearTo(c gputypes.Color) func(*Frame) error {
	return func(f *Frame) error {
		pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
			ColorAttachments: []wgpu.RenderPassColorAttachment{{
				View:       f.Color,
				LoadOp:     gputypes.LoadOpClear,
				StoreOp:    gputypes.StoreOpStore,
				ClearValue: c,
			}},
		})
		if err != nil {
			return err
		}
		return pass.End()
	}
}

func TestNewRejectsInvalidSize(t *testing.T) {
	for _, o := range []Options{{Width: 0, Height: 10}, {Width: 10, Height: -1}} {
		if _, err := New(context.Background(), o); err == nil {
			t.Fatalf("New(%+v) accepted an invalid size", o)
		}
	}
}

func TestNewHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(ctx, Options{Width: 4, Height: 4}); !errors.Is(err, context.Canceled) {
		t.Fatalf("New with cancelled ctx = %v, want context.Canceled", err)
	}
}

// TestAdapterErrorExposesSentinel: gogpu reports a missing adapter as a plain
// error on both backends (and never a nil adapter), so New must wrap that
// failure so the documented ErrNoAdapter is reachable through errors.Is
// while the original message is preserved.
func TestAdapterErrorExposesSentinel(t *testing.T) {
	cause := errors.New("no adapters available")
	err := adapterError(cause)
	if !errors.Is(err, ErrNoAdapter) {
		t.Fatalf("errors.Is(err, ErrNoAdapter) false for %v", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("original cause lost from %v", err)
	}
	var e *Error
	if !errors.As(err, &e) || e.Op != "request adapter" {
		t.Fatalf("err = %v, want *Error{Op: request adapter}", err)
	}
	if adapterError(nil) == nil || !errors.Is(adapterError(nil), ErrNoAdapter) {
		t.Fatal("nil adapter with nil error must still report ErrNoAdapter")
	}
}

func TestNewReportsAdapterAndSize(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 16, Height: 8, Label: "t"})
	info := r.AdapterInfo()
	t.Logf("adapter: %q backend=%v type=%v", info.Name, info.Backend, info.DeviceType)
	if info.Name == "" {
		t.Fatal("adapter name is empty")
	}
	if w, h := r.Size(); w != 16 || h != 8 {
		t.Fatalf("Size = %dx%d, want 16x8", w, h)
	}
}

func TestDoRunsWithDevice(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 4, Height: 4})
	var fromDo, fromFrame *wgpu.Device
	if err := r.Do(func(dev *wgpu.Device) error { fromDo = dev; return nil }); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := r.RenderToTexture(context.Background(), func(f *Frame) error {
		fromFrame = f.Device
		return clearTo(gputypes.Color{})(f)
	}); err != nil {
		t.Fatalf("RenderToTexture: %v", err)
	}
	if fromDo == nil || fromDo != fromFrame {
		t.Fatal("Do and Frame did not receive the same device")
	}
	want := errors.New("boom")
	if err := r.Do(func(*wgpu.Device) error { return want }); !errors.Is(err, want) {
		t.Fatalf("Do error = %v, want %v", err, want)
	}
}

func TestCloseIsIdempotentAndBlocksRender(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 4, Height: 4})
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := r.Render(context.Background(), clearTo(gputypes.Color{})); !errors.Is(err, ErrClosed) {
		t.Fatalf("Render after Close = %v, want ErrClosed", err)
	}
	if err := r.Resize(8, 8); !errors.Is(err, ErrClosed) {
		t.Fatalf("Resize after Close = %v, want ErrClosed", err)
	}
	if err := r.Do(func(*wgpu.Device) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Do after Close = %v, want ErrClosed", err)
	}
}

// expectSolid asserts every pixel of img is within ±1 of want.
func expectSolid(t *testing.T, img *image.NRGBA, want [4]byte) {
	t.Helper()
	b := img.Bounds()
	if img.Stride != b.Dx()*4 {
		t.Fatalf("stride = %d, want %d", img.Stride, b.Dx()*4)
	}
	for i := 0; i+3 < len(img.Pix); i += 4 {
		for c := range 4 {
			d := int(img.Pix[i+c]) - int(want[c])
			if d < -1 || d > 1 {
				t.Fatalf("pixel %d channel %d = %d, want %d±1 (first 8 bytes %v)", i/4, c, img.Pix[i+c], want[c], img.Pix[:8])
			}
		}
	}
}

func TestRenderSolidClear(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 33, Height: 7, Depth: true, Label: "clear"})
	img, st, err := r.Render(context.Background(), clearTo(gputypes.Color{R: 0.25, G: 0.5, B: 0.75, A: 1}))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got := img.Bounds(); got.Dx() != 33 || got.Dy() != 7 {
		t.Fatalf("bounds = %v", got)
	}
	expectSolid(t, img, [4]byte{64, 128, 191, 255})
	if st.Seq != 1 || st.Total <= 0 || st.BytesPerRow != 256 {
		t.Fatalf("stats = %+v", st)
	}
	if st.Draw+st.Submit+st.Map+st.Copy > st.Total {
		t.Fatalf("component times exceed total: %+v", st)
	}
}

func TestRenderSequenceAndFreshImages(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	a, sa, err := r.Render(context.Background(), clearTo(gputypes.Color{R: 1, A: 1}))
	if err != nil {
		t.Fatalf("first Render: %v", err)
	}
	b, sb, err := r.Render(context.Background(), clearTo(gputypes.Color{G: 1, A: 1}))
	if err != nil {
		t.Fatalf("second Render: %v", err)
	}
	if sa.Seq != 1 || sb.Seq != 2 {
		t.Fatalf("seq = %d,%d", sa.Seq, sb.Seq)
	}
	if &a.Pix[0] == &b.Pix[0] {
		t.Fatal("Render reused the previous image's buffer")
	}
	expectSolid(t, a, [4]byte{255, 0, 0, 255})
	expectSolid(t, b, [4]byte{0, 255, 0, 255})
}

func TestResizeChangesOutputAndNoOpsOnSameSize(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	if err := r.Resize(8, 8); err != nil {
		t.Fatalf("same-size Resize: %v", err)
	}
	if err := r.Resize(65, 3); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	img, st, err := r.Render(context.Background(), clearTo(gputypes.Color{B: 1, A: 1}))
	if err != nil {
		t.Fatalf("Render after Resize: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 65 || b.Dy() != 3 {
		t.Fatalf("bounds after Resize = %v", b)
	}
	if st.BytesPerRow != 512 {
		t.Fatalf("BytesPerRow = %d, want 512", st.BytesPerRow)
	}
	expectSolid(t, img, [4]byte{0, 0, 255, 255})
	if err := r.Resize(0, 4); err == nil {
		t.Fatal("Resize accepted zero width")
	}
}

func TestResizeFailureKeepsOldTarget(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	// 32768² RGBA is 4 GiB of staging buffer, beyond any default MaxBufferSize.
	// The browser reports creation failures asynchronously, so this also
	// covers the explicit limit check.
	if err := r.Resize(32768, 32768); err == nil {
		t.Fatal("Resize accepted a 4 GiB staging buffer")
	}
	if w, h := r.Size(); w != 8 || h != 8 {
		t.Fatalf("Size after failed Resize = %dx%d, want 8x8", w, h)
	}
	img, _, err := r.Render(context.Background(), clearTo(gputypes.Color{R: 1, G: 1, A: 1}))
	if err != nil {
		t.Fatalf("Render after failed Resize: %v", err)
	}
	expectSolid(t, img, [4]byte{255, 255, 0, 255})
}

func TestRenderCancelledContext(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := r.Render(ctx, clearTo(gputypes.Color{})); !errors.Is(err, context.Canceled) {
		t.Fatalf("Render with cancelled ctx = %v, want context.Canceled", err)
	}
	// The staging buffer must still be mappable afterwards.
	img, _, err := r.Render(context.Background(), clearTo(gputypes.Color{A: 1}))
	if err != nil {
		t.Fatalf("Render after cancelled Render: %v", err)
	}
	expectSolid(t, img, [4]byte{0, 0, 0, 255})
}

// TestRenderRecoversAfterManyDrawErrors guards against leaking the command
// encoder on the draw error path: Metal caps uncommitted command buffers per
// queue at 64, and the 65th CreateCommandEncoder blocks forever inside the
// FFI, wedging the executor thread.
func TestRenderRecoversAfterManyDrawErrors(t *testing.T) {
	if os.Getenv("GPUIMAGE_GPU_TEST") != "1" {
		t.Skip("set GPUIMAGE_GPU_TEST=1 to run GPU tests")
	}
	r, err := New(context.Background(), Options{Width: 8, Height: 8, Depth: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	boom := errors.New("boom")
	for i := range 70 {
		if _, _, err := r.Render(context.Background(), func(*Frame) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("draw error %d = %v, want boom", i, err)
		}
	}
	// A wedged executor never returns, so bound the wait instead of hanging
	// the whole test binary. On a hang the renderer is deliberately not
	// closed: Close would block on the same stuck thread.
	type result struct {
		img *image.NRGBA
		err error
	}
	done := make(chan result, 1)
	go func() {
		img, _, err := r.Render(context.Background(), clearTo(gputypes.Color{R: 1, A: 1}))
		done <- result{img, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Render after draw errors: %v", res.err)
		}
		expectSolid(t, res.img, [4]byte{255, 0, 0, 255})
		_ = r.Close()
	case <-time.After(15 * time.Second):
		t.Fatal("Render hung after 70 draw errors: the encoder is leaked on the draw error path")
	}
}

func TestRenderDrawErrorIsWrapped(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	boom := errors.New("boom")
	_, _, err := r.Render(context.Background(), func(*Frame) error { return boom })
	var e *Error
	if !errors.As(err, &e) || e.Op != "draw" || !errors.Is(err, boom) {
		t.Fatalf("draw error = %v, want *Error{Op: draw} wrapping boom", err)
	}
}

func TestExplicitReadPixels(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 19, Height: 13})
	st, err := r.RenderToTexture(context.Background(), clearTo(gputypes.Color{R: 1, A: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Map != 0 || st.Copy != 0 {
		t.Fatal("render-only path performed readback")
	}
	for range 2 {
		img, _, err := r.ReadPixels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := img.NRGBAAt(10, 6); got.R != 255 || got.G != 0 || got.A != 255 {
			t.Fatalf("readback pixel: %v", got)
		}
	}
}

// TestRenderHonoursDeadlineWhileQueued: a Render waiting behind another
// job must give up at its deadline instead of running late.
func TestRenderHonoursDeadlineWhileQueued(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	started, release := make(chan struct{}), make(chan struct{})
	blocked := make(chan error, 1)
	go func() {
		blocked <- r.Do(func(*wgpu.Device) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	time.AfterFunc(500*time.Millisecond, func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	drawn := false
	begin := time.Now()
	_, _, err := r.Render(ctx, func(f *Frame) error {
		drawn = true
		return clearTo(gputypes.Color{})(f)
	})
	waited := time.Since(begin)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Render queued past its deadline = %v, want context.DeadlineExceeded", err)
	}
	if drawn {
		t.Error("draw callback ran after the deadline")
	}
	if waited > 300*time.Millisecond {
		t.Errorf("Render returned after %v, want about 50ms", waited)
	}
	if err := <-blocked; err != nil {
		t.Fatalf("Do: %v", err)
	}
	// The renderer stays usable.
	img, _, err := r.Render(context.Background(), clearTo(gputypes.Color{G: 1, A: 1}))
	if err != nil {
		t.Fatalf("Render after an abandoned one: %v", err)
	}
	expectSolid(t, img, [4]byte{0, 255, 0, 255})
}

// TestRenderObservesCancellationAfterDraw: cancelling during the draw
// callback must not be reported as success.
func TestRenderObservesCancellationAfterDraw(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	ctx, cancel := context.WithCancel(context.Background())
	_, _, err := r.Render(ctx, func(f *Frame) error {
		defer cancel()
		return clearTo(gputypes.Color{R: 1, A: 1})(f)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Render cancelled during draw = %v, want context.Canceled", err)
	}
	img, _, err := r.Render(context.Background(), clearTo(gputypes.Color{B: 1, A: 1}))
	if err != nil {
		t.Fatalf("Render after a cancelled one: %v", err)
	}
	expectSolid(t, img, [4]byte{0, 0, 255, 255})
}

// TestRenderWithCancellableContext covers the map wait that can observe
// cancellation, which differs from the wait used for context.Background.
func TestRenderWithCancellableContext(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 65, Height: 9, Depth: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range 3 {
		img, _, err := r.Render(ctx, clearTo(gputypes.Color{R: 1, G: 1, A: 1}))
		if err != nil {
			t.Fatal(err)
		}
		expectSolid(t, img, [4]byte{255, 255, 0, 255})
	}
}

// TestReadPixelsAfterReadback: a standalone readback must work whether the
// previous frame was read back or only drawn.
func TestReadPixelsAfterReadback(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 19, Height: 13})
	ctx := context.Background()
	if _, _, err := r.Render(ctx, clearTo(gputypes.Color{G: 1, A: 1})); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		img, _, err := r.ReadPixels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expectSolid(t, img, [4]byte{0, 255, 0, 255})
	}
	// Drawing again after standalone readbacks still works, with and
	// without clearing.
	img, _, err := r.Render(ctx, clearTo(gputypes.Color{B: 1, A: 1}))
	if err != nil {
		t.Fatal(err)
	}
	expectSolid(t, img, [4]byte{0, 0, 255, 255})
}

// TestRenderCancelledDuringReadback sweeps deadlines across a frame's
// duration, so that some expire while the GPU is still working. Whatever
// the outcome, the next frame must read back correctly.
func TestRenderCancelledDuringReadback(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 2048, Height: 2048, Depth: true})
	background := context.Background()
	_, warm, err := r.Render(background, clearTo(gputypes.Color{A: 1}))
	if err != nil {
		t.Fatal(err)
	}
	const steps = 40
	var completed, cancelledEarly, cancelledWaiting int
	for i := range steps {
		ctx, cancel := context.WithTimeout(background, warm.Total*time.Duration(i)/steps)
		img, st, err := r.Render(ctx, clearTo(gputypes.Color{R: 1, A: 1}))
		cancel()
		switch {
		case err == nil:
			completed++
			expectSolid(t, img, [4]byte{255, 0, 0, 255})
		case !errors.Is(err, context.DeadlineExceeded):
			t.Fatalf("deadline %d/%d of %v: %v", i, steps, warm.Total, err)
		case st.Submit > 0:
			cancelledWaiting++
		default:
			cancelledEarly++
		}
		img, _, err = r.Render(background, clearTo(gputypes.Color{G: 1, A: 1}))
		if err != nil {
			t.Fatalf("Render after deadline %d/%d: %v", i, steps, err)
		}
		expectSolid(t, img, [4]byte{0, 255, 0, 255})
	}
	t.Logf("frame %v: %d completed, %d cancelled before submit, %d cancelled while waiting for the GPU",
		warm.Total, completed, cancelledEarly, cancelledWaiting)
	if cancelledWaiting == 0 {
		t.Log("no deadline expired during the map wait; that path was not exercised in this run")
	}
}

// TestRenderToTextureWithCancellableContext: submissions that do not wait
// for the GPU must not exhaust command buffers or lose frames.
func TestRenderToTextureWithCancellableContext(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 32, Height: 32, Depth: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := range 200 {
		c := gputypes.Color{R: 1, A: 1}
		if i == 199 {
			c = gputypes.Color{B: 1, A: 1}
		}
		if _, err := r.RenderToTexture(ctx, clearTo(c)); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	img, _, err := r.ReadPixels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expectSolid(t, img, [4]byte{0, 0, 255, 255})
}

// loadOnly opens a pass that keeps the target's contents and draws nothing.
func loadOnly(f *Frame) error {
	pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
		ColorAttachments: []wgpu.RenderPassColorAttachment{{
			View: f.Color, LoadOp: gputypes.LoadOpLoad, StoreOp: gputypes.StoreOpStore,
		}},
	})
	if err != nil {
		return err
	}
	return pass.End()
}

// TestRenderKeepsContentsWithLoad: a pass that loads the target must see the
// previous frame in every order of drawing and reading back.
func TestRenderKeepsContentsWithLoad(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 19, Height: 13})
	ctx := context.Background()
	red := [4]byte{255, 0, 0, 255}
	img, _, err := r.Render(ctx, clearTo(gputypes.Color{R: 1, A: 1}))
	if err != nil {
		t.Fatal(err)
	}
	expectSolid(t, img, red)
	for range 2 { // after a readback
		if img, _, err = r.Render(ctx, loadOnly); err != nil {
			t.Fatal(err)
		}
		expectSolid(t, img, red)
	}
	if _, err = r.RenderToTexture(ctx, loadOnly); err != nil { // draw only, after a readback
		t.Fatal(err)
	}
	if img, _, err = r.Render(ctx, loadOnly); err != nil { // after a draw only
		t.Fatal(err)
	}
	expectSolid(t, img, red)
	if img, _, err = r.ReadPixels(ctx); err != nil {
		t.Fatal(err)
	}
	expectSolid(t, img, red)
	if _, err = r.RenderToTexture(ctx, loadOnly); err != nil { // after a standalone readback
		t.Fatal(err)
	}
	if img, _, err = r.ReadPixels(ctx); err != nil {
		t.Fatal(err)
	}
	expectSolid(t, img, red)
}

func TestReadPixelsNeedsAFrame(t *testing.T) {
	r := newTestRenderer(t, Options{Width: 8, Height: 8})
	ctx := context.Background()
	if _, _, err := r.ReadPixels(ctx); err == nil {
		t.Fatal("ReadPixels before any draw succeeded")
	}
	if _, err := r.RenderToTexture(ctx, clearTo(gputypes.Color{G: 1, A: 1})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ReadPixels(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Resize(4, 4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ReadPixels(ctx); err == nil {
		t.Fatal("ReadPixels after Resize, before any draw, succeeded")
	}
}
