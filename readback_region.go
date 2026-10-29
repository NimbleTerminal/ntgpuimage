package ntgpuimage

import (
	"context"
	"encoding/binary"
	"errors"
	"image"
	"math"
	"time"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
)

// ReadPixelsInto copies the current main target into dst without allocating a
// pixel slice. dst must match Size; nonzero bounds and padded strides are valid.
// The caller must not access dst until this call returns.
func (r *Renderer) ReadPixelsInto(ctx context.Context, dst *image.NRGBA) (Stats, error) {
	if dst == nil {
		return Stats{}, &Error{Op: "read pixels into", Err: errors.New("nil destination")}
	}
	_, st, err := r.execute(ctx, nil, true, dst)
	return st, err
}

// ReadRegion reads a nonempty rectangle contained in the main color target.
// The returned image has origin (0, 0). Only the requested pixels are copied to
// a temporary staging buffer. Call after Render or RenderToTexture.
func (r *Renderer) ReadRegion(ctx context.Context, region image.Rectangle) (*image.NRGBA, Stats, error) {
	var img *image.NRGBA
	st, err := r.readRegion(ctx, region, false, func(src []byte, stride int) error {
		img = image.NewNRGBA(image.Rect(0, 0, region.Dx(), region.Dy()))
		return copyImageRows(img, src, region.Dx(), region.Dy(), stride)
	})
	if err != nil {
		return nil, st, err
	}
	return img, st, nil
}

// ReadDepth reads one raw depth value at (x, y) from the main target, with a
// top-left origin. Options.DepthFormat must be Depth32Float and the draw pass
// must store depth. WebGPU requires copying the complete depth subresource;
// this method maps that copy but only transfers the selected float to Go.
// The result is the stored depth, without projection or reversed-Z conversion.
func (r *Renderer) ReadDepth(ctx context.Context, x, y int) (float32, Stats, error) {
	var depth float32
	st, err := r.readRegion(ctx, image.Rect(x, y, x+1, y+1), true, func(src []byte, stride int) error {
		offset := y*stride + x*4
		depth = math.Float32frombits(binary.LittleEndian.Uint32(src[offset : offset+4]))
		return nil
	})
	return depth, st, err
}

func (r *Renderer) readRegion(ctx context.Context, region image.Rectangle, depth bool, consume func([]byte, int) error) (st Stats, err error) {
	if r.closed.Load() {
		return st, ErrClosed
	}
	runErr := r.exec.run(ctx, func() {
		start := time.Now()
		defer func() { st.Total = time.Since(start) }()
		if r.device == nil {
			err = ErrClosed
			return
		}
		if err = ctx.Err(); err != nil {
			return
		}
		t := &r.target
		st.Seq = r.seq
		if !t.drawn {
			err = &Error{Op: "read region", Err: errors.New("no frame drawn since the target was created")}
			return
		}
		if region.Empty() || !region.In(image.Rect(0, 0, t.width, t.height)) {
			err = &Error{Op: "read region", Err: errors.New("region is empty or outside the target")}
			return
		}
		tex, aspect := t.color, gputypes.TextureAspectAll
		origin := wgpu.Origin3D{X: uint32(region.Min.X), Y: uint32(region.Min.Y)}
		w, h := region.Dx(), region.Dy()
		if depth {
			if r.depthFormat != gputypes.TextureFormatDepth32Float {
				err = &Error{Op: "read depth", Err: errors.New("Depth32Float attachment required")}
				return
			}
			tex, aspect = t.depthTex, gputypes.TextureAspectDepthOnly
			origin, w, h = wgpu.Origin3D{}, t.width, t.height
		}
		stride := alignedBytesPerRow(w)
		st.BytesPerRow = stride
		staging, createErr := r.newStaging(&target{height: h, bytesPerRow: stride})
		if createErr != nil {
			err = createErr
			return
		}
		defer staging.Release()
		enc, createErr := r.device.CreateCommandEncoder(&wgpu.CommandEncoderDescriptor{Label: r.label + ":region"})
		if createErr != nil {
			err = &Error{Op: "create encoder", Err: createErr}
			return
		}
		// A separate submission lets wgpu's usage tracker transition the source.
		enc.CopyTextureToBuffer(tex, staging, []wgpu.BufferTextureCopy{{
			BufferLayout: wgpu.ImageDataLayout{BytesPerRow: uint32(stride), RowsPerImage: uint32(h)},
			TextureBase:  wgpu.ImageCopyTexture{Texture: tex, Origin: origin, Aspect: aspect},
			Size:         wgpu.Extent3D{Width: uint32(w), Height: uint32(h), DepthOrArrayLayers: 1},
		}})
		submitStart := time.Now()
		cmd, submitErr := r.submit(enc)
		if submitErr != nil {
			err = submitErr
			return
		}
		st.Submit = time.Since(submitStart)
		st.Map, st.Copy, err = mapStaging(ctx, r.device, staging, uint64(stride*h), func(src []byte) error { return consume(src, stride) })
		if err == nil {
			r.device.FreeCommandBuffer(cmd)
		}
	})
	if runErr != nil {
		return st, runErr
	}
	return st, err
}
