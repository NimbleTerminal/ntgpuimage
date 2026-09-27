package gpuimage

import "errors"

// Error wraps a gogpu/wgpu failure with the step that produced it.
type Error struct {
	Op  string
	Err error
}

func (e *Error) Error() string { return "gpuimage: " + e.Op + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// ErrClosed is returned by every Renderer method after Close.
var ErrClosed = errors.New("gpuimage: renderer closed")

// ErrNoAdapter is returned by New when the instance reports no GPU adapter.
var ErrNoAdapter = errors.New("gpuimage: no GPU adapter available")
