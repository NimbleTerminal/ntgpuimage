package ntgpuimage

import (
	"context"
	"errors"

	"github.com/gogpu/wgpu"
)

// RenderTexture records draw and mip generation into a caller-owned encoder.
// enc must belong to this renderer's device and have no open pass. This method
// neither finishes nor submits enc. Later passes in enc may sample the texture.
// Submit enc before using the texture in another encoder, an update, or readback.
// Keep the texture and renderer alive until the host finishes its GPU work.
// A mipmapped texture may be rendered only once in a given encoder.
//
// The callback follows Frame.RenderTo rules. An error after draw starts requires
// discarding the encoder. Validation errors before draw leave it usable.
// If an encoder containing a successful call is abandoned or fails submission,
// recreate the affected textures before using them again: their tracked layouts
// assume the recorded commands execute. Calls must follow host submission order.
func (r *Renderer) RenderTexture(enc *wgpu.CommandEncoder, t *Texture, draw func(*Frame) error) error {
	if enc == nil || draw == nil {
		return &Error{Op: "render texture", Err: errors.New("nil encoder or draw callback")}
	}
	if r.closed.Load() {
		return ErrClosed
	}
	var err error
	runErr := r.exec.run(context.Background(), func() {
		if r.device == nil {
			err = ErrClosed
			return
		}
		if t != nil && t.r == r && t.generatesMips() && t.externalEncoder == enc {
			err = &Error{Op: "render texture", Err: errors.New("a mipmapped render target can be drawn into once per encoder")}
			return
		}
		r.seq++
		f := &Frame{Device: r.device, Encoder: enc, Seq: r.seq, r: r}
		err = f.RenderTo(t, draw)
		if err == nil && t.generatesMips() {
			t.externalEncoder = enc
		}
	})
	if runErr != nil {
		return runErr
	}
	return err
}
