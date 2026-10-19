# Changelog

## Unreleased

### Mipmapped render targets

- `TextureOptions.Mipmaps` now works with `RenderTarget`, including `SRGB`,
  `Float16`, and `Depth`. After each `Frame.RenderTo`, and after `Update`,
  every lower level is regenerated on the GPU in the same submission: one
  bilinear tap per texel at the destination texel centers, which is the 2×2
  box average for even sizes. `SRGB` targets average in linear light.
  Generated levels stay within 2/255 per channel of `MipChain` over a 64×32
  chain on Metal, Chrome, and Vulkan.
- `UploadTextureLevels` accepts `RenderTarget` with more than one level;
  `RenderTo` then regenerates the supplied chain. `UpdateLevels` uploads
  supplied levels unchanged until the next `RenderTo` or `Update`.
- Depth attachments keep one level. `ReadPixels` still returns level 0.
- A mipmapped render target can be drawn into once per frame; a second
  `RenderTo` in the same frame returns an error before its callback runs.
- Because gogpu v0.34.5 reports every texture view at the base texture's
  size, levels are drawn into persistent per-level scratch targets and copied
  into place. The scratch targets hold about a third of level 0. When
  upstream sizes views by `BaseMipLevel`, the copies can go.
- Checked with `task ci`, on Metal (Apple M5 Max), in Chrome, and on Vulkan
  (NVIDIA GB10) under the validation layer.

### Mip generation performance

- `MipChain`, and automatic mipmaps on upload and `Update`, reduce each level
  with a direct 2×2 box when both axes halve exactly, including the 1×N and
  N×1 tail of a chain. Odd dimensions keep the general area-overlap filter.
  Output is byte-identical to v0.3.0 for every level and option.
- sRGB encoding looks up rounding thresholds instead of calling `math.Pow`
  per channel, with the exact encoder used near every boundary. Output is
  identical for every input.
- A 512×512 sRGB alpha-weighted chain takes about 1.4 ms instead of 5.3 ms
  on an Apple M5 Max; 1024×1024 about 5.4 ms instead of 21 ms. The
  ntcharts-osm map texture replacement benchmark (nt3d v0.2.0, ntcharts3d
  v0.3.1) drops from about 15.4 ms to 10.4 ms. Allocations are unchanged. See
  `benchmarks/2026-10-18-mipchain.txt`.
- Checked with `task ci`, on Metal (Apple M5 Max), in Chrome, and on Vulkan
  (NVIDIA GB10) under the validation layer.
- Release guidance: tag as `v0.3.1`. No API, semantic, or GPU upload contract
  changes; nt3d and ntcharts3d need no code changes. Consumers pick it up by
  bumping the `nimbleterminal.dev/ntgpuimage` requirement (directly, or via a
  new nt3d patch release that requires it).

## `0.3.0` — 2026-10-14

### Texture formats

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
- Checked on Metal, in Chrome, and on Vulkan (NVIDIA GB10) under the
  validation layer.

### Mipmaps and samplers

- Add CPU-generated mipmaps on image upload and update, plus `MipChain` for
  CPU renderers and caller alpha-coverage adjustments. sRGB RGB averages in
  linear light; alpha weighting is optional for transparent color images.
- Add `UploadTextureLevels`, `UpdateLevels`, and `MipLevelCount` for explicit
  full or partial chains. Supplied texels and coverage-adjusted alpha are
  preserved. Readback always loads level 0 independently of sampler state.
- Render-target/Float16 mip generation is not included in this image path.
- Add repeat and mirror-repeat addressing per texture axis.
- Add `TextureOptions.Anisotropy` to all texture samplers: 0 or 1 disables
  anisotropy, 2–16 sets the maximum. Values above 16 and values above 1 with
  nearest filtering are rejected.
- Checked on Metal, in Chrome, and on Mesa Vulkan llvmpipe (a software
  adapter) under the validation layer.

### Known issues

- On Vulkan, gogpu v0.34.5 can give a new texture the stale usage of a
  closed one, so its first submission starts with a barrier from the wrong
  layout. The validation layer reports it; pixels were correct on the
  NVIDIA GB10. It also affects v0.2.0. See [VALIDATION.md](VALIDATION.md).
- On Vulkan, `New` intermittently fails with `vkMapMemory returned null
  pointer` on the NVIDIA GB10, about one run in ten, as v0.2.0 does.

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
