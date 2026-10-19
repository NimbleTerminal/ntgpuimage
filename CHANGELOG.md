# Changelog

## `0.4.0` — 2026-10-19

### Mipmapped render targets

- `TextureOptions.Mipmaps` works with `RenderTarget`, including `SRGB`,
  `Float16`, and `Depth`.
- After each `Frame.RenderTo` and `Update`, the GPU regenerates every lower
  level in the same submission. Each texel is one bilinear tap at the
  destination texel center, a 2×2 box for even sizes. `SRGB` targets average
  in linear light. Levels match `MipChain` within 2/255 per channel over an
  exactly-halving 64×32 chain.
- `UpdateLevels` and `UploadTextureLevels` store the supplied levels. The next
  `RenderTo` or `Update` regenerates them.
- Depth attachments keep one level. `ReadPixels` returns level 0.
- A mipmapped render target accepts one `RenderTo` per frame. A second call
  returns an error before its callback runs.
- Levels render into per-level scratch textures and are copied into place,
  because gogpu v0.34.5 sizes every view at the base texture. Scratch holds
  about a third of level 0.

### Mip generation performance

- `MipChain` and automatic mipmaps use a direct 2×2 box when both axes halve
  exactly, including the 1×N and N×1 tail. Odd sizes keep the area filter.
  Output is byte-identical to v0.3.0.
- sRGB encoding uses a threshold table instead of `math.Pow`. Output is
  identical.
- On an Apple M5 Max, a 512×512 sRGB alpha-weighted chain drops from 5.3 ms to
  1.4 ms, and 1024×1024 from 21 ms to 5.4 ms. The ntcharts-osm texture
  replacement benchmark drops from 15.4 ms to 10.4 ms. Allocations are
  unchanged. See `benchmarks/2026-10-18-mipchain.txt`.

Checked with `task ci`, on Metal (Apple M5 Max), in Chrome, and on Vulkan
(NVIDIA GB10) under the validation layer.

## `0.3.0` — 2026-10-14

### Texture formats

- `TextureOptions.SRGB` stores a texture as `RGBA8UnormSrgb`. Upload, `Update`,
  and `ReadPixels` exchange sRGB-encoded bytes. Shaders sample linear values.
  Drawing into an `SRGB` target encodes the output and blends in linear light.
- `TextureOptions.Float16` makes an `RGBA16Float` render target, with or
  without `Depth`.
- `Texture.Format` reports the storage format.

### Mipmaps and samplers

- CPU-generated mipmaps on upload and `Update`, plus `MipChain` for callers.
  sRGB averages in linear light. Alpha weighting is optional.
- `UploadTextureLevels`, `UpdateLevels`, and `MipLevelCount` for explicit
  chains. Supplied texels are preserved. Readback always returns level 0.
- Mipmaps are not available on render targets or `Float16` textures.
- Repeat and mirror-repeat addressing per texture axis.
- `TextureOptions.Anisotropy`: 0 or 1 disables, 2–16 sets the maximum. Values
  above 16, or above 1 with nearest filtering, are rejected.

Checked on Metal, in Chrome, and on Vulkan (NVIDIA GB10 and Mesa llvmpipe)
under the validation layer.

### Known issues

- On Vulkan, gogpu v0.34.5 can give a new texture the stale usage of a closed
  one, so its first submission uses the wrong layout. The validation layer
  reports it; pixels were correct. Also affects v0.2.0. See
  [VALIDATION.md](VALIDATION.md).
- On Vulkan, `New` intermittently fails with `vkMapMemory returned null
  pointer` on the NVIDIA GB10, about one run in ten. Also affects v0.2.0.

## `0.2.0` — 2026-10-13

- Moved to `nimbleterminal.dev/ntgpuimage` (GitHub `NimbleTerminal/ntgpuimage`).
  The package is `ntgpuimage` and error strings carry that prefix.
  `github.com/NimbleMarkets/go-gpuimage` v0.1.0 stays available without updates.
- `Renderer.NewTexture` allocates a blank texture. `TextureOptions.RenderTarget`
  makes it drawable with `Frame.RenderTo`; `TextureOptions.Depth` adds a depth
  attachment. One pass can render into a texture that a later pass samples.
- `Texture.ReadPixels` returns an error for a texture never written or drawn.
- Two-pass example: `examples/twopass`.

Checked on Metal, in Chrome, and on Vulkan under the validation layer.

## `0.1.0` — 2026-09-27

- Offscreen WebGPU rendering on native platforms and in the browser.
- Persistent RGBA8 target with optional depth and resize.
- Custom shaders and pipelines through upstream WebGPU types.
- Image upload, sampling, update, and readback as `image.NRGBA`.
- GPU work on one OS thread, with `Do` for setup and cleanup.
- Context cancellation while waiting to draw or read pixels.
- Device-limit checks before allocating targets or copying pixels.
- Frame timing statistics and adapter information.
- Examples for clearing a target and sampling an image.
- Native, browser, and Vulkan validation tasks.
- Clear-and-readback benchmarks with Metal and Vulkan results.
