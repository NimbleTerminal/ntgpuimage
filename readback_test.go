package gpuimage

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestAlignedBytesPerRow(t *testing.T) {
	cases := []struct {
		name  string
		width int
		want  int
	}{
		{"one-pixel", 1, 256},
		{"aligned-64", 64, 256},
		{"just-over", 65, 512},
		{"odd-33", 33, 256},
		{"wide-1280", 1280, 5120},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := alignedBytesPerRow(c.width); got != c.want {
				t.Fatalf("alignedBytesPerRow(%d) = %d, want %d", c.width, got, c.want)
			}
		})
	}
}

// paddedSource builds a padded staging-buffer image where pixel (x,y) has
// bytes {x, y, x^y, 255} and every pad byte is 0xEE so a copy that reads
// into the padding is detectable.
func paddedSource(width, height, bytesPerRow int) []byte {
	src := bytes.Repeat([]byte{0xEE}, bytesPerRow*height)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			o := y*bytesPerRow + x*4
			src[o], src[o+1], src[o+2], src[o+3] = byte(x), byte(y), byte(x^y), 255
		}
	}
	return src
}

func TestDepad(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
	}{
		{"1x1", 1, 1},
		{"odd-33x7", 33, 7},
		{"aligned-64x3", 64, 3},
		{"just-over-65x2", 65, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bpr := alignedBytesPerRow(c.width)
			src := paddedSource(c.width, c.height, bpr)
			dst := make([]byte, c.width*c.height*4)
			if err := depad(dst, src, c.width, c.height, bpr); err != nil {
				t.Fatalf("depad: %v", err)
			}
			for y := 0; y < c.height; y++ {
				for x := 0; x < c.width; x++ {
					o := (y*c.width + x) * 4
					want := []byte{byte(x), byte(y), byte(x ^ y), 255}
					if !bytes.Equal(dst[o:o+4], want) {
						t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, dst[o:o+4], want)
					}
				}
			}
			if bytes.IndexByte(dst, 0xEE) >= 0 {
				t.Fatal("padding byte leaked into destination")
			}
		})
	}
}

func TestDepadRejectsShortBuffers(t *testing.T) {
	bpr := alignedBytesPerRow(10)
	src := make([]byte, bpr*4)
	if err := depad(make([]byte, 10*4*4-1), src, 10, 4, bpr); err == nil {
		t.Fatal("short dst accepted")
	}
	// The last row may omit its padding, so trim into its pixel bytes.
	if err := depad(make([]byte, 10*4*4), src[:bpr*3+10*4-1], 10, 4, bpr); err == nil {
		t.Fatal("short src accepted")
	}
	if err := depad(make([]byte, 10*4*4), src[:bpr*3+10*4], 10, 4, bpr); err != nil {
		t.Fatalf("src with an unpadded last row rejected: %v", err)
	}
	if err := depad(make([]byte, 10*4*4), src, 10, 4, 39); err == nil {
		t.Fatal("bytesPerRow smaller than a row accepted")
	}
}

func TestErrorFormatsWithPrefix(t *testing.T) {
	e := &Error{Op: "submit", Err: ErrNoAdapter}
	if got, want := e.Error(), "gpuimage: submit: gpuimage: no GPU adapter available"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if e.Unwrap() != ErrNoAdapter {
		t.Fatal("Unwrap did not return the wrapped error")
	}
}

func TestPollUntil(t *testing.T) {
	calls := 0
	err := pollUntil(context.Background(), func() (bool, error) {
		calls++
		return calls == 4, nil
	})
	if err != nil || calls != 4 {
		t.Fatalf("pollUntil: calls=%d err=%v", calls, err)
	}
	boom := errors.New("boom")
	if err := pollUntil(context.Background(), func() (bool, error) { return true, boom }); !errors.Is(err, boom) {
		t.Fatalf("pollUntil error = %v, want boom", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	begin := time.Now()
	err = pollUntil(ctx, func() (bool, error) { return false, nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pollUntil past its deadline = %v, want context.DeadlineExceeded", err)
	}
	if waited := time.Since(begin); waited > time.Second {
		t.Fatalf("pollUntil returned after %v", waited)
	}
}
