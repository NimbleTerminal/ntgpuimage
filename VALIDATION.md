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
unpacks the `vulkan-validationlayers` package under `~/.cache/go-gpuimage`
on the test machine without installing it system-wide.

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
