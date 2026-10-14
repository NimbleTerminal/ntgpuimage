package ntgpuimage

import (
	"context"
	"errors"
	"fmt"
	"image"

	"github.com/gogpu/gputypes"
)

// UploadTextureLevels uploads a caller-supplied mip chain, with level 0 first.
// Each following level must halve each dimension (round down, minimum 1).
// A partial chain is allowed; the sampler clamps to its last level. All bytes,
// including coverage-adjusted alpha, are uploaded unchanged. SRGB describes
// how the supplied RGB bytes are encoded; no generation or conversion occurs.
// Mipmaps must be false. Use UpdateLevels to replace this chain.
func (r *Renderer) UploadTextureLevels(levels []image.Image, opts TextureOptions) (*Texture, error) {
	const op = "upload texture levels"
	if r.closed.Load() {
		return nil, ErrClosed
	}
	if err := opts.validate(op); err != nil {
		return nil, err
	}
	if opts.Float16 {
		return nil, &Error{Op: op, Err: errors.New("Float16 levels cannot be uploaded")}
	}
	if opts.Mipmaps {
		return nil, &Error{Op: op, Err: errors.New("supplied levels exclude automatic Mipmaps")}
	}
	filter, err := opts.filter(op)
	if err != nil {
		return nil, err
	}
	w, h, err := validateLevels(op, levels)
	if err != nil {
		return nil, err
	}
	if len(levels) > 1 && opts.RenderTarget {
		return nil, &Error{Op: op, Err: errors.New("mipmapped render targets are not yet supported")}
	}
	if err := r.checkTextureSize(op, w, h); err != nil {
		return nil, err
	}
	return r.createTexture(packLevels(levels), w, h, filter, opts, len(levels))
}

// UpdateLevels replaces every allocated level without reallocating the texture
// or bind group. The count and dimensions must match the original chain.
// Pixels are copied before returning. No alpha or color-space conversion is
// applied to the packed NRGBA bytes. This also accepts a MipChain result.
func (t *Texture) UpdateLevels(levels []image.Image) error {
	const op = "update texture levels"
	if t.r.closed.Load() {
		return ErrClosed
	}
	if t.format == gputypes.TextureFormatRGBA16Float {
		return &Error{Op: op, Err: errors.New("a Float16 texture cannot be updated; draw it with Frame.RenderTo")}
	}
	if len(levels) != t.mipLevels {
		return &Error{Op: op, Err: fmt.Errorf("got %d levels, want %d", len(levels), t.mipLevels)}
	}
	w, h, err := validateLevels(op, levels)
	if err != nil {
		return err
	}
	if w != t.width || h != t.height {
		return &Error{Op: op, Err: fmt.Errorf("size %dx%d differs from %dx%d", w, h, t.width, t.height)}
	}
	return t.updatePackedLevels(packLevels(levels))
}

func (t *Texture) updatePackedLevels(pixels [][]byte) error {
	var err error
	if runErr := t.r.exec.run(context.Background(), func() {
		if t.closed || t.r.device == nil {
			err = ErrClosed
			return
		}
		err = t.writeLevels(pixels)
	}); runErr != nil {
		return runErr
	}
	return err
}

// Validate all bounds before packing any pixels, including later levels.
func validateLevels(op string, levels []image.Image) (int, int, error) {
	if len(levels) == 0 {
		return 0, 0, &Error{Op: op, Err: errors.New("empty mip chain")}
	}
	w, h, err := imageSize(op, levels[0])
	if err != nil {
		return 0, 0, err
	}
	if len(levels) > mipLevelCount(w, h) {
		return 0, 0, &Error{Op: op, Err: errors.New("too many mip levels")}
	}
	mw, mh := w, h
	for i, level := range levels {
		lw, lh, err := imageSize(op, level)
		if err != nil {
			return 0, 0, err
		}
		if lw != mw || lh != mh {
			return 0, 0, &Error{Op: op, Err: fmt.Errorf("level %d size %dx%d, want %dx%d", i, lw, lh, mw, mh)}
		}
		mw, mh = max(1, mw/2), max(1, mh/2)
	}
	return w, h, nil
}

func packLevels(levels []image.Image) [][]byte {
	pixels := make([][]byte, len(levels))
	for i, level := range levels {
		pixels[i] = packPixels(level)
	}
	return pixels
}
