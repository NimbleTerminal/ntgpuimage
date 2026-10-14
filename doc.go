// Package ntgpuimage provides offscreen WebGPU rendering, texture upload and
// sampling, and optional pixel readback into Go images. It uses gogpu/wgpu's
// native and browser backends and exposes upstream types for custom drawing.
//
// # Pixels
//
// The main target and default textures are RGBA8Unorm without color-space
// conversion. Readback returns the target's bytes as image.NRGBA, so a
// shader that writes premultiplied color must be un-premultiplied by the
// caller. Uploads store straight (non-premultiplied) alpha.
//
// TextureOptions.SRGB stores a texture as RGBA8UnormSrgb. It exchanges the
// same sRGB-encoded bytes with Go, but shaders sample linear values that
// were decoded before filtering, and drawing into it encodes linear output
// and blends in linear light. TextureOptions.Float16 stores a render target
// as RGBA16Float.
//
// TextureOptions.Mipmaps generates CPU mip levels on upload and update.
// UploadTextureLevels and Texture.UpdateLevels accept caller-supplied chains;
// MipChain exposes the same generator for CPU renderers and alpha-coverage
// adjustments. AddressModeU/V select clamp, repeat, or mirror-repeat sampling.
// Anisotropy selects 2–16x anisotropic filtering with linear Filter, or 0/1
// to disable it, on any texture.
// Mipmaps currently exclude render targets and Float16. ReadPixels returns
// level 0 regardless of the sampler's mip or addressing settings.
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
// # Render targets
//
// A Texture created with TextureOptions.RenderTarget can be drawn into with
// Frame.RenderTo and sampled by later passes in the same frame or in later
// frames, with no readback in between. RenderTo records the layout
// transitions Vulkan needs around the pass, and ReadPixels on a drawn
// texture reads it back through a GPU blit, because gogpu's usage tracker
// cannot be told about sampling; see VALIDATION.md. Render targets have a
// fixed size and are released by Close like other textures.
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
