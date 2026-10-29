package ntgpuimage

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"testing"
	"time"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

func TestBorrowedValidation(t *testing.T) {
	if _, err := NewFromDevice(nil, Options{}); err == nil {
		t.Fatal("accepted nil device")
	}
	if _, err := NewFromProvider(nil, Options{}); err == nil {
		t.Fatal("accepted nil provider")
	}
	for _, opts := range []Options{{Width: 1}, {Height: -1}, {DepthFormat: gputypes.TextureFormatRGBA8Unorm}} {
		if err := validateOptions(opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
	if err := validateOptions(Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectExecutorCancellationAndSerialization(t *testing.T) {
	e := newDirectExecutor()
	defer e.stop()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { defer close(done); _ = e.run(context.Background(), func() { close(started); <-release }) }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := e.run(ctx, func() { t.Error("ran while occupied") }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued run: %v", err)
	}
	close(release)
	<-done
	// Recovering here verifies the callback executes on this goroutine, and
	// that a panic releases the serialization token.
	func() {
		defer func() {
			if recover() != "caller" {
				t.Error("callback did not panic on caller")
			}
		}()
		_ = e.run(context.Background(), func() { panic("caller") })
	}()
	if err := e.run(context.Background(), func() {}); err != nil {
		t.Fatal(err)
	}
	e.stop()
	if err := e.run(context.Background(), func() { t.Error("ran after close") }); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestCopyImageRowsSubimage(t *testing.T) {
	parent := image.NewNRGBA(image.Rect(5, 6, 10, 10))
	for i := range parent.Pix {
		parent.Pix[i] = 17
	}
	dst := parent.SubImage(image.Rect(6, 7, 9, 9)).(*image.NRGBA)
	src := make([]byte, 512)
	for y := range 2 {
		for x := range 12 {
			src[y*256+x] = byte(y + 1)
		}
	}
	if err := copyImageRows(dst, src, 3, 2, 256); err != nil {
		t.Fatal(err)
	}
	for y := 6; y < 10; y++ {
		for x := 5; x < 10; x++ {
			want := uint8(17)
			if image.Pt(x, y).In(dst.Rect) {
				want = uint8(y - 6)
			}
			if got := parent.NRGBAAt(x, y).R; got != want {
				t.Fatalf("(%d,%d)=%d, want %d", x, y, got, want)
			}
		}
	}
	for _, bad := range []*image.NRGBA{nil, {Rect: image.Rect(0, 0, 3, 2), Stride: 12, Pix: make([]byte, 23)}, {Rect: image.Rect(0, 0, 3, 2), Stride: -1}} {
		if err := validateDestination(bad, 3, 2); err == nil {
			t.Fatal("accepted malformed destination")
		}
	}
}

func TestBorrowedDeviceReadbackAndEncoder(t *testing.T) {
	owner := newTestRenderer(t, Options{Width: 4, Height: 4})
	ctx := context.Background()
	err := owner.Do(func(dev *wgpu.Device) error {
		r, err := NewFromDevice(dev, Options{DepthFormat: gputypes.TextureFormatDepth32Float})
		if err != nil {
			return err
		}
		defer r.Close()
		if w, h := r.Size(); w != 0 || h != 0 {
			return fmt.Errorf("initial size %dx%d", w, h)
		}
		if _, _, err := r.Render(ctx, clearTo(gputypes.Color{A: 1})); err == nil {
			return errors.New("render accepted missing target")
		}
		tex, err := r.NewTexture(8, 4, TextureOptions{RenderTarget: true, Mipmaps: true})
		if err != nil {
			return err
		}
		enc, err := dev.CreateCommandEncoder(nil)
		if err != nil {
			return err
		}
		if err := r.RenderTexture(enc, tex, clearTo(gputypes.Color{R: 1, A: 1})); err != nil {
			enc.DiscardEncoding()
			return err
		}
		if err := r.RenderTexture(enc, tex, clearTo(gputypes.Color{G: 1, A: 1})); err == nil {
			enc.DiscardEncoding()
			return errors.New("accepted repeat mip draw")
		}
		cmd, err := r.submit(enc)
		if err != nil {
			return err
		}
		img, err := tex.ReadPixels(ctx)
		if err != nil {
			return err
		}
		dev.FreeCommandBuffer(cmd)
		if got := img.NRGBAAt(0, 0); got != (color.NRGBA{R: 255, A: 255}) {
			return fmt.Errorf("host draw: %v", got)
		}
		// Read every generated level on the same host thread.
		for level := 1; level < tex.MipLevelCount(); level++ {
			img, err := tex.readLevel(ctx, level)
			if err != nil {
				return err
			}
			if img.NRGBAAt(0, 0) != (color.NRGBA{R: 255, A: 255}) {
				return fmt.Errorf("mip %d: %v", level, img.NRGBAAt(0, 0))
			}
		}
		if err := r.Resize(7, 3); err != nil {
			return err
		}
		_, err = r.RenderToTexture(ctx, func(f *Frame) error {
			pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
				ColorAttachments:       []wgpu.RenderPassColorAttachment{{View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore, ClearValue: gputypes.Color{G: 1, A: 1}}},
				DepthStencilAttachment: &wgpu.RenderPassDepthStencilAttachment{View: f.Depth, DepthLoadOp: gputypes.LoadOpClear, DepthStoreOp: gputypes.StoreOpStore, DepthClearValue: 0.25, StencilReadOnly: true},
			})
			if err != nil {
				return err
			}
			return pass.End()
		})
		if err != nil {
			return err
		}
		img, st, err := r.ReadRegion(ctx, image.Rect(2, 1, 5, 3))
		if err != nil {
			return err
		}
		if img.Bounds() != image.Rect(0, 0, 3, 2) || img.NRGBAAt(2, 1) != (color.NRGBA{G: 255, A: 255}) || st.BytesPerRow != 256 {
			return fmt.Errorf("region: %v %+v", img, st)
		}
		if _, _, err := r.ReadRegion(ctx, image.Rect(-1, 0, 1, 1)); err == nil {
			return errors.New("accepted outside region")
		}
		dst := image.NewNRGBA(image.Rect(10, 20, 19, 25)).SubImage(image.Rect(11, 21, 18, 24)).(*image.NRGBA)
		before := &dst.Pix[0]
		if _, err := r.ReadPixelsInto(ctx, dst); err != nil {
			return err
		}
		if &dst.Pix[0] != before || dst.NRGBAAt(17, 23) != (color.NRGBA{G: 255, A: 255}) {
			return errors.New("reusable readback mismatch")
		}
		depth, _, err := r.ReadDepth(ctx, 3, 1)
		if err != nil {
			return err
		}
		if depth != 0.25 {
			return fmt.Errorf("depth = %g", depth)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, _, err := r.ReadRegion(cancelled, image.Rect(0, 0, 1, 1)); !errors.Is(err, context.Canceled) {
			return fmt.Errorf("cancelled region: %v", err)
		}
		if err := r.Close(); err != nil {
			return err
		}
		if err := r.Resize(1, 1); !errors.Is(err, ErrClosed) {
			return fmt.Errorf("closed resize: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Closing the borrower must leave the owner's device and queue operational.
	if _, _, err := owner.Render(ctx, clearTo(gputypes.Color{B: 1, A: 1})); err != nil {
		t.Fatal(err)
	}
}
