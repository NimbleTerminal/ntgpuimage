package ntgpuimage

import (
	"context"
	"fmt"
	"image"
	"time"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

// rowAlign is WebGPU's required alignment for bytesPerRow in
// texture-to-buffer copies.
const rowAlign = 256

// alignedBytesPerRow returns the padded stride for an RGBA8 row of width
// pixels.
func alignedBytesPerRow(width int) int {
	return (width*4 + rowAlign - 1) / rowAlign * rowAlign
}

// depad copies width*4 bytes from each padded row of src into the tightly
// packed dst. It is the only per-pixel CPU work in this package.
func depad(dst, src []byte, width, height, bytesPerRow int) error {
	row := width * 4
	if bytesPerRow < row {
		return fmt.Errorf("ntgpuimage: bytesPerRow %d smaller than row %d", bytesPerRow, row)
	}
	if len(dst) < row*height {
		return fmt.Errorf("ntgpuimage: dst has %d bytes, need %d", len(dst), row*height)
	}
	if need := bytesPerRow*(height-1) + row; len(src) < need {
		return fmt.Errorf("ntgpuimage: src has %d bytes, need %d", len(src), need)
	}
	for y := range height {
		copy(dst[y*row:(y+1)*row], src[y*bytesPerRow:y*bytesPerRow+row])
	}
	return nil
}

// transition declares that a texture moves from one usage to another inside
// an encoder.
func transition(enc *wgpu.CommandEncoder, tex *wgpu.Texture, from, to gputypes.TextureUsage) {
	enc.TransitionTextures([]wgpu.TextureBarrier{{
		Texture: tex,
		Range:   wgpu.TextureRange{Aspect: gputypes.TextureAspectAll, MipLevelCount: 1, ArrayLayerCount: 1},
		Usage:   wgpu.TextureUsageTransition{OldUsage: from, NewUsage: to},
	}})
}

// encodeReadback records a copy of an RGBA8 texture into a staging buffer
// whose rows are stride bytes apart.
func encodeReadback(enc *wgpu.CommandEncoder, tex *wgpu.Texture, staging *wgpu.Buffer, w, h, stride int) {
	enc.CopyTextureToBuffer(tex, staging, []wgpu.BufferTextureCopy{{
		BufferLayout: wgpu.ImageDataLayout{BytesPerRow: uint32(stride), RowsPerImage: uint32(h)},
		TextureBase:  wgpu.ImageCopyTexture{Texture: tex},
		Size:         wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
	}})
}

// mapImage maps a staging buffer filled by a submitted encodeReadback, copies
// its rows into a new image, and unmaps it. It reports the time spent waiting
// for the map and copying. A cancelled ctx returns ctx.Err() and leaves the
// buffer unmapped; the caller must release it after failure because the GPU
// may still be executing the copy and tracking its abandoned map request.
func mapImage(ctx context.Context, dev *wgpu.Device, staging *wgpu.Buffer, w, h, stride int) (_ *image.NRGBA, mapped, copied time.Duration, err error) {
	return mapImageInto(ctx, dev, staging, w, h, stride, nil)
}

func mapImageInto(ctx context.Context, dev *wgpu.Device, staging *wgpu.Buffer, w, h, stride int, dst *image.NRGBA) (*image.NRGBA, time.Duration, time.Duration, error) {
	img := dst
	mapped, copied, err := mapStaging(ctx, dev, staging, uint64(stride*h), func(src []byte) error {
		if img == nil {
			img = image.NewNRGBA(image.Rect(0, 0, w, h))
		}
		return copyImageRows(img, src, w, h, stride)
	})
	if err != nil {
		return nil, mapped, copied, err
	}
	return img, mapped, copied, nil
}

// mapStaging consumes mapped bytes before releasing the range and unmapping.
// After a failed map the caller must release the buffer rather than reuse it.
func mapStaging(ctx context.Context, dev *wgpu.Device, staging *wgpu.Buffer, size uint64, consume func([]byte) error) (mapped, copied time.Duration, err error) {
	start := time.Now()
	pending, err := staging.MapAsync(wgpu.MapModeRead, 0, size)
	if err != nil {
		return 0, 0, &Error{Op: "map", Err: err}
	}
	err = waitMapped(ctx, dev, pending)
	pending.Release()
	if err != nil {
		_ = staging.Unmap()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, 0, ctxErr
		}
		return 0, 0, &Error{Op: "map", Err: err}
	}
	mapped = time.Since(start)
	start = time.Now()
	rng, err := staging.MappedRange(0, size)
	if err != nil {
		_ = staging.Unmap()
		return mapped, 0, &Error{Op: "mapped range", Err: err}
	}
	err = consume(rng.Bytes())
	rng.Release()
	if unmapErr := staging.Unmap(); unmapErr != nil && err == nil {
		err = &Error{Op: "unmap", Err: unmapErr}
	}
	return mapped, time.Since(start), err
}

// Bounds for the delay between polls in pollUntil. A frame takes about a
// millisecond, so the first polls are much shorter than that.
const (
	pollMinDelay = 20 * time.Microsecond
	pollMaxDelay = time.Millisecond
)

// pollUntil calls poll until it reports done, waiting a little longer after
// each attempt. It returns poll's error, or ctx.Err() once ctx ends.
func pollUntil(ctx context.Context, poll func() (done bool, err error)) error {
	timer := time.NewTimer(pollMinDelay)
	defer timer.Stop()
	for delay := pollMinDelay; ; delay = min(2*delay, pollMaxDelay) {
		if done, err := poll(); done {
			return err
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// validateDestination accepts subimages and padded strides, but rejects malformed
// public image.NRGBA values before any mapping or GPU submission.
func validateDestination(dst *image.NRGBA, w, h int) error {
	if dst == nil || w <= 0 || h <= 0 || dst.Rect.Dx() != w || dst.Rect.Dy() != h || dst.Stride < w*4 {
		return &Error{Op: "read pixels into", Err: fmt.Errorf("destination must be a %dx%d NRGBA image with a valid stride", w, h)}
	}
	if len(dst.Pix) < w*4 || (h > 1 && dst.Stride > (len(dst.Pix)-w*4)/(h-1)) {
		return &Error{Op: "read pixels into", Err: fmt.Errorf("destination pixel buffer is too short")}
	}
	return nil
}

func copyImageRows(dst *image.NRGBA, src []byte, w, h, stride int) error {
	if err := validateDestination(dst, w, h); err != nil {
		return err
	}
	if stride < w*4 || len(src) < stride*(h-1)+w*4 {
		return fmt.Errorf("ntgpuimage: source pixel buffer is too short")
	}
	for y := range h {
		copy(dst.Pix[y*dst.Stride:y*dst.Stride+w*4], src[y*stride:y*stride+w*4])
	}
	return nil
}
