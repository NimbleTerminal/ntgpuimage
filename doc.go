// Package ntgpuimage provides offscreen WebGPU rendering, texture upload and
// sampling, and optional pixel readback into Go images. It uses gogpu/wgpu's
// native and browser backends and exposes upstream types for custom drawing.
//
// # Pixels
//
// Targets and textures are RGBA8Unorm without color-space conversion.
// Readback returns the target's bytes as image.NRGBA, so a shader that
// writes premultiplied color must be un-premultiplied by the caller.
// Uploads store straight (non-premultiplied) alpha.
//
// # Threading
//
// A Renderer owns one goroutine pinned to an OS thread. Every GPU call —
// device creation, target allocation, the caller's draw callback, the
// copy, the map, and all releases — runs on that goroutine. Use Renderer.Do
// to run pipeline and buffer setup on it, and Frame.Device inside a draw
// callback.
//
// gogpu/wgpu documents devices as safe for concurrent use and command
// encoders as not. The single thread is this package's choice: it keeps
// one frame in flight, keeps the wait for a mapped buffer on the thread
// that submitted the work, and gives thread-bound backends such as GLES a
// stable thread. Code that receives the device must not retain it for use
// on other goroutines.
//
// # Browser
//
// On GOOS=js the map step awaits a JavaScript promise, which is fine from
// any Go goroutine except one that is itself running inside a JS callback
// (that would deadlock the event loop). Start rendering from a Go goroutine.
//
// When attaching Frame.Depth (a depth-only Depth24Plus view) to a render
// pass, set StencilReadOnly: true on the RenderPassDepthStencilAttachment.
// gogpu's browser backend otherwise emits stencilLoadOp/stencilStoreOp,
// which WebGPU rejects for a format with no stencil aspect; native backends
// ignore the flag for a depth-only attachment.
package ntgpuimage
