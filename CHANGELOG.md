# Changelog

## `0.2.0` — 2026-10-13

- Moved to `nimbleterminal.dev/ntgpuimage` (GitHub `NimbleTerminal/ntgpuimage`).
  The Go package is now `ntgpuimage` and error strings carry that prefix.
  `github.com/NimbleMarkets/go-gpuimage` v0.1.0 remains available but will
  not receive updates.
- `Renderer.NewTexture` allocates a blank texture, and
  `TextureOptions.RenderTarget` makes a texture drawable with
  `Frame.RenderTo`, so one pass can render into a texture that a later pass
  samples, with no readback in between. `TextureOptions.Depth` adds a depth
  attachment to such a texture. Checked on Metal, in Chrome, and on Vulkan
  under the validation layer.
- `Texture.ReadPixels` reports an error for a texture that has never been
  written or drawn into.
- A two-pass example: `examples/twopass`.

## `0.1.0` — 2026-09-27

Initial version:

- Offscreen WebGPU rendering on native platforms and in the browser.
- A persistent RGBA8 target with optional depth and resize support.
- Custom shaders and pipelines through upstream WebGPU types.
- Image upload, sampling, updates, and readback as `image.NRGBA`.
- GPU work serialized on one OS thread, with `Do` for setup and cleanup.
- Context cancellation while waiting to draw or read pixels.
- Device-limit checks before allocating targets or copying image pixels.
- Frame timing statistics and adapter information.
- Examples for clearing a target and sampling an uploaded image.
- Native, browser, and Vulkan validation tasks.
- Clear-and-readback benchmarks with recorded Metal and Vulkan results.
