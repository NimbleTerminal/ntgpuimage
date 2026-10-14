# Changelog

## Unreleased

- Add repeat and mirror-repeat addressing per texture axis.
- Add `TextureOptions.Anisotropy` to all texture samplers: 0 or 1 disables
  anisotropy, 2–16 sets the maximum. Values above 16 and values above 1 with
  nearest filtering are rejected.
- Add CPU-generated mipmaps on image upload and update, plus `MipChain` for
  CPU renderers and caller alpha-coverage adjustments. sRGB RGB averages in
  linear light; alpha weighting is optional for transparent color images.
- Add `UploadTextureLevels`, `UpdateLevels`, and `MipLevelCount` for explicit
  full or partial chains. Supplied texels and coverage-adjusted alpha are
  preserved. Readback always loads level 0 independently of sampler state.
- Render-target/Float16 mip generation is not included in this image path.

## `0.3.0` — 2026-10-14

- `TextureOptions.SRGB` stores a texture as `RGBA8UnormSrgb`. Uploads,
  `Update` and `ReadPixels` exchange the same sRGB-encoded bytes as before.
  Shaders sample linear values that were decoded before filtering. Drawing
  into an `SRGB` render target encodes the shader's linear output and blends
  in linear light. `ReadPixels` on a drawn `SRGB` texture blits through an
  sRGB scratch target, so it returns the stored bytes.
- `TextureOptions.Float16` makes an `RGBA16Float` render target, with or
  without `Depth`, for a linear scene that a later pass resolves through
  `BindGroup` with `textureSample` or `textureLoad`.
- `Texture.Format` reports the storage format, for a pipeline that draws
  into the texture.
- Checked on Metal, in Chrome, and on Vulkan under the validation layer.

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
