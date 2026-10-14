# Validation

Results recorded on 2026-09-27 with wgpu v0.34.5 and naga v0.19.0.
These are recorded test results, not guarantees for every device.

## Test systems

| Backend | System |
| --- | --- |
| Metal | Apple M3 Max, macOS, Go 1.27.1 |
| Browser WebGPU | Chrome 154 on the same Mac; adapter reported `vendor=apple architecture=metal-3 fallback=false` |
| Vulkan | NVIDIA GB10, driver 580.178.04, Ubuntu 24.04 arm64, Go 1.26.8, Khronos validation layer 1.4.328 |

## Results

| Check | Result |
| --- | --- |
| `task ci`: race tests, native and WASM vet, WASM build, Node helper tests | Pass |
| `task test-gpu` on Metal | 37 pass, 1 skip (`TestDerivedUVShader`) |
| `task test-browser` in headless Chrome | 39 pass, 0 skip |
| `task test-vulkan` | 38 pass, 0 skip, 0 validation errors |
| Both examples on Metal | Pass; PNG output inspected |

Node tests have no WebGPU device. Builds check compilation only. GPU tests
log the adapter used; a software adapter does not test hardware GPU behavior.
CI runs without a GPU. The hardware and browser tests were run separately.

## What the tests cover

- Rendering with and without depth, resizing, and frame sequence numbers.
- Texture upload, sampling, updates, and readback.
- A failed resize preserving the old target.
- Rejection of oversized uploads and mismatched updates before reading pixels.
- Cancellation while queued, after drawing, and during readback.
- A successful frame after cancellation or a failed draw callback.
- Repeated readback and drawing with the previous frame's contents.
- Rejection of readback before the first draw to a target.

## Vulkan layouts

Correct pixels alone do not prove that texture layouts are valid. The
Vulkan tests also run with the Khronos validation layer. `task test-vulkan`
fails if the layer reports an error, does not load, or the tests use a
backend other than Vulkan.

Validation caught three layout errors that are now fixed:

- Standalone readback declared a render-attachment transition after the
  target had already moved to the copy-source layout.
- Uploaded texture readback copied from the shader-read layout. It now
  transitions to CopySrc for the copy and back afterward.
- A render pass that loaded the previous frame could start in CopySrc.
  `Render` now transitions back before the draw callback when needed.

To repeat the tests:

```sh
task test-vulkan                         # on Linux with Vulkan
task test-vulkan-remote HOST=gpu-host    # replace gpu-host with your SSH host
```

The script uses an installed validation layer. If none is available, it
unpacks the `vulkan-validationlayers` package under `~/.cache/ntgpuimage`
on the test machine without installing it system-wide.

### Render-target textures

Run on 2026-10-13 on the Vulkan system above with the same wgpu, naga and
validation layer: `task test-vulkan-remote`, 51 tests, 0 validation
errors, after these findings in wgpu v0.34.5:

- A render pass leaves an attachment that also has `TextureBinding` usage
  in `GENERAL` (`hal/vulkan/command.go`, `offscreenFinalLayout`), while
  sampler descriptors are written as `SHADER_READ_ONLY_OPTIMAL`. Sampling
  straight after the pass reported `VUID-VkDescriptorImageInfo-imageLayout-00344`.
  `Frame.RenderTo` now moves the texture to the sampled layout after the
  pass, and back to `GENERAL` before the next one, which a pass with
  `LoadOp` `Load` expects. The barrier names `StorageBinding`, the only
  usage gogpu maps to `GENERAL`.
- The usage tracker records the attachment usage and, before any later
  submission that copies from the texture, records its own transition from
  `COLOR_ATTACHMENT_OPTIMAL`. Nothing in the command buffer runs before it,
  so a direct copy from a drawn texture reported `VUID-vkCmdDraw-None-09600`
  on every readback and on the next pass. `Texture.ReadPixels` on a drawn
  texture now samples it into a scratch target without `TextureBinding`
  usage and copies from that, following the main target's pattern.
- A `Render` frame whose draw callback never attaches the main target left
  it undefined before the transition that precedes the readback copy. The
  target is now moved to the attachment layout before the first draw.
- A pipeline layout released right after pipeline creation is still used
  by `vkCmdBindDescriptorSets` on Vulkan. Metal did not object.

The three examples, including `examples/twopass`, also ran on the same
system under the layer with 0 validation errors.

### sRGB and Float16 textures

Run on 2026-10-14 on the systems above, with the same wgpu and naga:
`task test-gpu` on Metal, 54 pass and 1 skip (`TestDerivedUVShader`);
`task test-browser`, 56 pass; `task test-vulkan-remote`, 55 pass and 0
validation errors; `task ci` passes.

- `TestSRGBTextureFiltersInLinearLight` samples halfway between a black and
  a white texel. An `SRGB` texture gives 0.5 linear, re-encoded 188; a plain
  one gives encoded 0.5, 128.
- `TestSRGBTextureRoundTrip` reads all 256 byte values back unchanged after
  an upload, after `Update`, and after a pass has drawn into the texture,
  with and without depth, which covers both the copy and the blit path.
- `TestRenderIntoSRGBTarget` writes linear 0.5, and separately blends white
  at half alpha over black. Both read back 188 from an `SRGB` target and
  128 from a plain one.
- `TestFloat16RenderTarget` keeps a value of 2.0 through `textureLoad` and
  clamps it on readback.

When the readback blit for an `SRGB` texture wrote to an `RGBA8Unorm`
scratch target, the round trip returned decoded values, for example 26 for
a stored 90. The blit now writes to an sRGB scratch target, which encodes
the texels again.

Two upstream problems showed up on Vulkan. Neither comes from the new
formats:

- The first version of `TestSRGBTextureRoundTrip` shared one renderer
  across its cases and reported `VUID-vkCmdDraw-None-09600`: a submission
  expected a texture in `TRANSFER_SRC_OPTIMAL` while it was still
  `UNDEFINED`. `Texture.Release` frees the texture's tracker index
  ([texture_native.go#L67-L75](https://github.com/gogpu/wgpu/blob/v0.34.5/texture_native.go#L67-L75)),
  but nothing calls `DeviceTracker.RemoveTexture`
  ([device_tracker.go#L106-L110](https://github.com/gogpu/wgpu/blob/v0.34.5/core/device_tracker.go#L106-L110)),
  so the next texture given that index inherits the last usage of the
  closed one. Its first submission then starts with a barrier from that
  usage. The same error reproduces with plain `RGBA8Unorm` textures and the
  v0.2.0 API: read one back by copy, close it, then create, update and draw
  into another. It disappears when the closed texture was never copied
  from. The test now gives each case its own renderer; the library does not
  yet work around the defect.
- `New` fails about one run in ten, in a different test each time, with
  `create staging buffer: ... vkMapMemory returned null pointer (BUG-VK-001)`
  ([device.go#L421](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/vulkan/device.go#L421)).
  Unmodified v0.2.0 fails the same way, once in ten runs. The 55-pass run
  above is one without it.

### Uploaded mipmaps and sampler addressing

Run on 2026-10-14 with the pinned wgpu v0.34.5:

- `task ci` passes (race tests, native/WASM vet, WASM build, Node tests).
- The full native GPU suite passes on an Apple M5 Max Metal adapter, with the
  existing `TestDerivedUVShader` skip. The Chrome WebGPU suite passes on the same
  machine. Final focused tests for partial chains and independent addressing
  also pass on both.
- The mip upload/update, minified checker, address-axis, and sRGB round-trip
  tests pass on Mesa Vulkan llvmpipe in an Ubuntu 24.04 arm64 Docker container
  with the Khronos validation layer enabled and no validation errors. The
  adapter reports CPU; this does not establish hardware Vulkan support for mips.

Tests cover explicit mip selection, `textureSampleGrad`, trilinear blending,
nearest mip selection, clamping to the last supplied level of a partial chain,
repeat across a bilinear seam, negative/mirrored coordinates on each axis, and
regeneration after Update. A 64x64 repeating checker minified beyond its last
level resolves to gray for both plain and sRGB textures. Supplied per-level alpha
is preserved. CPU tests cover linear-light sRGB averaging, optional alpha
weighting, rebased image bounds, narrow and odd dimensions, and rejecting an
invalid lower level before packing any pixels.

Render-target mip generation is excluded. In the pinned Vulkan backend,
`CreateTextureView` stores the base texture dimensions without reducing them by
`BaseMipLevel`, so rendering to smaller mip views needs an upstream fix and
separate validation of subresource transitions.

## Cancellation

Tests cover calls that expire while queued and contexts cancelled during
drawing. Native readback polls when the context can be cancelled; a
background context uses a blocking wait.

A cancelled native map exposed an upstream bug: the next map on the same
staging buffer could fail with `map canceled`. The library replaces the
buffer after a failed or cancelled wait.

`TestRenderCancelledDuringReadback` tries 40 deadlines while rendering a
2048 by 2048 frame, then checks that the next frame succeeds. In five Metal
runs, 2 or 3 deadlines per run expired during the map wait. Chrome runs
caught between 0 and 2. The inspected Vulkan run caught none. A passing
run therefore does not always exercise cancellation during mapping.

On Metal at 640 by 400 over 2,000 frames, readback took 0.56 ms per frame
with `context.Background()` and 0.67 ms with a cancellable context.

## Metal shader workaround

naga v0.19.0 generates invalid Metal source for arithmetic on a component
of an indexed `let` or `const` array, such as `p[i].x * 0.5`. Its bounds
check lacks parentheses, so multiplication applies to the fallback value.

The examples and texture tests list UV coordinates explicitly to avoid
this bug. `TestDerivedUVShader` uses the affected expression and skips on
Metal when pipeline creation fails. The test passes in Chrome and on Vulkan.
Once it passes on Metal, the workaround can be removed.

## Not tested

- DX12, GLES, and browsers other than Chrome.
- Vulkan devices other than the NVIDIA GB10.
- Cancellation during a Vulkan map wait; the inspected run completed too
  quickly to exercise it.
