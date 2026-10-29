# Design

`ntgpuimage` manages offscreen render targets, image textures, GPU execution,
and pixel readback. Callers use WebGPU types directly for shaders,
pipelines, buffers, and drawing.

It uses gogpu/wgpu without a window framework. The related libraries
reviewed below do not provide the same combination of a persistent color
and depth target, custom shaders, image upload, and native and browser
readback.

The comparison records source inspection and tests from 2026-09-27.
Version and branch comparisons describe that date. See
[VALIDATION.md](VALIDATION.md) for the library's test results.

## Versions reviewed

| Module | Used here | Tag reviewed | Main reviewed |
| --- | --- | --- | --- |
| [gogpu/wgpu](https://github.com/gogpu/wgpu) | v0.34.5 | v0.34.5 | `052a7e5`, identical to the tag |
| [gogpu/naga](https://github.com/gogpu/naga) | v0.19.0 | v0.19.0 | `a3d9917`, one documentation commit |
| [gogpu/gpucontext](https://github.com/gogpu/gpucontext) | v0.31.3 (indirect) | v0.31.3 | `a50e6f9`, identical to the tag |
| [gogpu/gogpu](https://github.com/gogpu/gogpu) | not used | v0.54.0, requires wgpu v0.34.2 | `12620c9` |
| [gogpu/gg](https://github.com/gogpu/gg) | not used | v0.52.5, requires wgpu v0.31.6 | `bd563f4`, still wgpu v0.31.6 |

The review found no newer wgpu implementation to use. The main-branch
changes in gogpu and gg did not affect this library.

## Comparison

"Ran" means executed on an Apple M3 Max, on Metal unless stated. Everything
else comes from reading source.

### Windowless rendering with custom shaders and depth

| Project | Finding |
| --- | --- |
| wgpu | Raw primitives only. One example, [`examples/triangle-headless`](https://github.com/gogpu/wgpu/blob/v0.34.5/examples/triangle-headless/main.go), renders color without depth. Ran: it fails on Metal with `texture usage conflict: existing 32 incompatible with 4`, because it copies the attachment without a transition ([main.go#L245](https://github.com/gogpu/wgpu/blob/v0.34.5/examples/triangle-headless/main.go#L245)). |
| wgpu | [`HeadlessSurfaceTarget`](https://github.com/gogpu/wgpu/blob/v0.34.5/surface_target.go#L54-L65) is implemented only by the software backend. Metal, Vulkan, DX12, GLES, and the browser reject it. |
| gogpu | [`NewHeadlessRenderer`](https://github.com/gogpu/gogpu/blob/v0.54.0/renderer.go#L298-L349) and [`RenderToImage`](https://github.com/gogpu/gogpu/blob/v0.54.0/renderer.go#L1751-L1806) exist and default to the software backend. The target is BGRA8Unorm, created per call, with no depth attachment and no resize. Ran: on Metal a custom pass recorded through `Context.CommandEncoder()` is dropped, because `RenderToImage` never submits the frame encoder. |
| gg | Draws only its own 2D primitives ([accelerator.go#L104-L140](https://github.com/gogpu/gg/blob/v0.52.5/accelerator.go#L104-L140)). Its depth-stencil texture is internal. |
| ntgpuimage | Persistent RGBA8Unorm target with optional Depth24Plus, resized in place, drawn by caller pipelines. |

### Image upload, samplers, updates, and alpha

| Project | Finding |
| --- | --- |
| wgpu | [`Queue.WriteTexture`](https://github.com/gogpu/wgpu/blob/v0.34.5/queue_native.go#L344) only. It pads rows internally, so a tight `4*width` stride is accepted. No `image.Image` helper, no sampler or bind group convenience, and no documented texture alpha convention. |
| gogpu | [`NewTextureFromImage`, `UpdateData`, `UpdateRegion`](https://github.com/gogpu/gogpu/blob/v0.54.0/texture.go#L208-L269) are methods on `gogpu.Renderer` and use its private device and private bind group layout. Alpha is premultiplied ([texture.go#L251-L260](https://github.com/gogpu/gogpu/blob/v0.54.0/texture.go#L251-L260)). |
| gg | Internal image cache; uploads premultiplied RGBA8Unorm ([context_image.go#L389-L390](https://github.com/gogpu/gg/blob/v0.52.5/context_image.go#L389-L390)). No in-place update. Fixed bilinear sampler. |
| ntgpuimage | `UploadTexture` from any `image.Image`, straight alpha, linear or nearest, in-place `Update`, exported bind group and layout. |

### Readback, row alignment, asynchronous mapping, cancellation

| Project | Finding |
| --- | --- |
| wgpu | No texture-to-pixels helper; `Queue.ReadBuffer` was removed ([CHANGELOG.md#L2017](https://github.com/gogpu/wgpu/blob/v0.34.5/CHANGELOG.md#L2017)). The 256-byte row alignment is not validated natively, and callers must align and de-pad themselves. |
| wgpu | [`Surface.ReadPixels`](https://github.com/gogpu/wgpu/blob/v0.34.5/surface_native.go#L487-L515) requires [`hal.PixelReader`](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/resource.go#L176-L186), which only [the software surface](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/software/resource.go#L179) implements. [The browser build always returns an error](https://github.com/gogpu/wgpu/blob/v0.34.5/surface_browser.go#L247-L257). It reads a surface framebuffer, never an arbitrary texture. |
| wgpu | [`Buffer.Map`](https://github.com/gogpu/wgpu/blob/v0.34.5/buffer.go#L111-L151) blocks with a context, but natively it polls the device from a second goroutine ([buffer.go#L144-L146](https://github.com/gogpu/wgpu/blob/v0.34.5/buffer.go#L144-L146)). `MapAsync` with `MapPending.Wait(ctx)` supports cancellation; a cancelled wait leaves the buffer pending until `Unmap` ([map_pending_browser.go#L48-L53](https://github.com/gogpu/wgpu/blob/v0.34.5/map_pending_browser.go#L48-L53)). |
| wgpu | In the browser [`Device.Poll` is a no-op](https://github.com/gogpu/wgpu/blob/v0.34.5/device_browser.go#L338-L340). Nothing keeps the page ticking while a map is pending. |
| gogpu | One private helper for its own target ([renderer.go#L1810-L1902](https://github.com/gogpu/gogpu/blob/v0.54.0/renderer.go#L1810-L1902)). No context. It checks `MapPending.Status()` once after polling, which cannot succeed where polling is a no-op. |
| gg | Internal, blocking, `context.Background()` ([render_session.go#L2670-L2769](https://github.com/gogpu/gg/blob/v0.52.5/internal/gpu/render_session.go#L2670-L2769)), and bound to gg's resolve texture. |
| ntgpuimage | One shared path: copy, `MapAsync`, poll on the executor thread, cancellable wait, `Unmap` on cancel, de-pad into a new `image.NRGBA`, plus a `requestAnimationFrame` pump in the browser. |

### Ownership, shutdown, resizing, threads

| Project | Finding |
| --- | --- |
| wgpu | Documents instance, adapter, and device as safe for concurrent use and encoders as not ([doc.go#L31-L34](https://github.com/gogpu/wgpu/blob/v0.34.5/doc.go#L31-L34)). Metal autorelease pools pin the OS thread for their own scope ([hal/metal/objc.go#L489-L504](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/metal/objc.go#L489-L504)). Its render-thread helper is in `internal/thread` and cannot be imported. |
| gogpu | Owns instance, adapter, and device. Importing the root package locks the main goroutine to its thread at init on macOS ([init.go#L28-L32](https://github.com/gogpu/gogpu/blob/v0.54.0/internal/platform/darwin/init.go#L28-L32)). `RenderToImage` is documented as not safe for concurrent use. |
| gg | One process-global accelerator bound to one device. Never releases a device it was given. No executor. |
| gpucontext | Interfaces and opaque handles. It states no threading rule. |
| ntgpuimage | One device per `Renderer`, all calls serialized on one pinned goroutine, a failed `Resize` keeps the old target, `Close` releases everything including textures still open. |

The executor serializes frames and keeps map waits on the submitting
thread. This is a design choice, not a Metal requirement.

### Native Metal and browser WASM

| Project | Finding |
| --- | --- |
| wgpu | Texture creation, render passes with depth, `WriteTexture`, `CopyTextureToBuffer`, and buffer mapping are implemented for Metal, Vulkan, DX12, GLES, software, and the browser. Only Metal, software, and Chrome were run. The browser backend is `*_browser.go` plus `internal/browser`; it has no usage tracking, and [`PopErrorScope` panics as unimplemented](https://github.com/gogpu/wgpu/blob/v0.34.5/device_browser.go#L317-L327). |
| gogpu | Compiles for `js/wasm`. The headless readback was not run in a browser. |
| gg | The root package compiles for `js/wasm` as a CPU rasterizer. `gg/gpu` does not: it imports `wgpu/core`, which is native only. |
| ntgpuimage | Ran on Metal and in headless Chrome 154 on the same machine (`vendor=apple architecture=metal-3`, not a fallback adapter). |

### Sharing a device, and interoperating with gg

| Project | Finding |
| --- | --- |
| gpucontext | [`DeviceProvider`](https://github.com/gogpu/gpucontext/blob/v0.31.3/device_provider.go#L31-L69) returns opaque handles. wgpu converts them with [`DeviceFromHandle` and `DeviceToHandle`](https://github.com/gogpu/wgpu/blob/v0.34.5/gpucontext_helpers.go#L15-L77). |
| gogpu | Shares its device from host to library only, and only after `App.Run` starts. It cannot adopt an external device. |
| gg | Accepts a device through [`gpu.SetDeviceProvider`](https://github.com/gogpu/gg/blob/v0.52.5/gpu/gpu.go#L41-L52). Created alone, it requests only a Vulkan adapter ([gpu_shared.go#L446-L449](https://github.com/gogpu/gg/blob/v0.52.5/internal/gpu/gpu_shared.go#L446-L449)), so on macOS it falls back to CPU. |
| gg | **Blocker:** `gg/gpu` fails to compile with wgpu v0.34.5 on both v0.52.5 and main, because wgpu v0.34.0 changed `Draw`, `DrawIndexed`, `SetViewport`, and `SetScissorRect` to struct parameters. A module importing both this library and `gg/gpu` cannot build. |
| gg | Ran with wgpu v0.31.6: on a host-owned Metal device gg drew shapes and text into its own offscreen texture. Its targets are hard-coded BGRA8Unorm, premultiplied, with 4x MSAA ([gpu_textures.go#L54-L62](https://github.com/gogpu/gg/blob/v0.52.5/internal/gpu/gpu_textures.go#L54-L62)). |

With these versions, one way to use gg is to draw with CPU gg, whose root
package builds with wgpu v0.34.5 and for `js/wasm`, un-premultiply its
pixmap, and pass it to `UploadTexture` or `Texture.Update`.

## Upstream APIs

| API | Use |
| --- | --- |
| `Queue.WriteTexture` | Used. Its internal row padding lets uploads pass a tight stride. |
| `Buffer.MapAsync`, `MapPending.Wait`, `Buffer.Unmap` | Used for readback. After cancellation, the library unmaps and replaces the buffer to avoid an upstream bug described below. |
| `Device.Poll(PollPoll)`, `MapPending.Status` | Used natively with cancellable contexts, because `Poll(PollWait)` cannot be interrupted. |
| `CommandEncoder.TransitionTextures` | Used around a draw that is followed by a readback in the same encoder, around a copy from a sampled texture, and around a `Frame.RenderTo` pass. On Vulkan a render pass leaves an attachment that also has `TextureBinding` usage in `GENERAL` ([command.go#L1602-L1607](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/vulkan/command.go#L1602-L1607)) while sampler descriptors name `SHADER_READ_ONLY_OPTIMAL` ([device.go#L1121](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/vulkan/device.go#L1121)), so `RenderTo` moves the texture to `GENERAL` before the pass and to the sampled layout after it. The only usage gogpu maps to `GENERAL` is `StorageBinding`, which the barrier names for that reason. Explicit transitions do not update the usage scope unless a copy follows ([encoder_native.go#L262-L290](https://github.com/gogpu/wgpu/blob/v0.34.5/encoder_native.go#L262-L290)), so a drawn texture is read back by blitting it into a scratch target rather than copying from it. Checked on Vulkan under the validation layer. Ran: without the first, `Finish` fails with a usage conflict. It is a no-op on Metal and in the browser. A draw alone or a readback alone declares nothing, because gogpu moves the texture from the previous submission's usage at submit. Checked on Vulkan under the validation layer. |
| `CommandEncoder.DiscardEncoding` | Used on a failed draw callback. |
| `Device.Limits` | Used to reject oversized targets before creation. |
| `Device.FreeCommandBuffer` | Called after the GPU has finished. `Submit` now recycles encoders itself, so the call looks redundant on Metal, Vulkan, GLES, and software. DX12 was not verified, so it stays. After a cancelled or failed wait it is skipped, since the GPU may still be using the buffer. |
| `Buffer.Map` | Not used. It polls from a second goroutine and has no browser pump. |
| `HeadlessSurfaceTarget`, `Surface.ReadPixels` | Not used. Software backend only. |
| `gpucontext.DeviceProvider` | Accepted by `NewFromProvider`; host threading rules remain the caller's responsibility. |
| gogpu and gg helpers | Not importable, or incompatible in format, alpha, and wgpu version. |

## Scope

The library provides:

- Device creation and cleanup through `New` and `Close`.
- A GPU executor with `Do` for setup and cleanup.
- A persistent color target, optional depth, and `Resize`.
- `Render`, with frame handles and timing statistics.
- Pixel readback with row alignment and cancellation.
- Image upload, sampling, updates, and texture readback.
- CPU-generated and caller-supplied mip chains for 8-bit images, with repeat and mirror-repeat samplers.
- `RenderToTexture` and `ReadPixels` for reading only selected frames.
- `NewTexture` with `RenderTarget`, and `Frame.RenderTo` for drawing into a texture that later passes sample.
- `RGBA8UnormSrgb` textures (`TextureOptions.SRGB`) for sampling, filtering, drawing and blending in linear light, and `RGBA16Float` render targets (`TextureOptions.Float16`).
- Adapter information and errors that callers can inspect.

It does not provide a scene engine, chart types, SVG parsing, an atlas
manager, MSAA, arbitrary texture formats, or wrappers around WebGPU types.

## Accepting an externally owned device

`NewFromDevice(dev, opts)` and `NewFromProvider(provider, opts)` borrow a
host-owned wgpu device. The renderer releases only resources it creates;
the host keeps its device alive until the renderer is closed and its GPU
work has finished. Provider handles must originate from wgpu.

Borrowed renderers start no goroutine. A cancellable serialization lock runs
work, callbacks, and releases directly on the caller's goroutine. The host
is responsible for calling on its GPU thread and coordinating other device
users. Adapter-selection options are ignored. The owning `New` constructor
continues using a dedicated OS-thread executor.

Zero dimensions defer the main target until `Resize`. Texture uploads and
`RenderTexture` work without that target. `RenderTexture` reuses `Frame.RenderTo`
and its mip-generation and layout transitions, leaving encoder submission to
the host. Successful recordings must be submitted in order before subsequent
texture use. Abandoning them requires recreating affected textures because
layout tracking assumes execution. A mipmapped texture is drawn at most once
per encoder, and resources must remain alive until the GPU is finished.

Readback shares the same map/wait path for whole images, reusable images,
color rectangles, and Depth32Float picking. Color regions use a region-sized
staging allocation. WebGPU requires depth copies to cover the full subresource;
`ReadDepth` maps that copy and extracts one float without allocating a full Go
image. Default depth remains Depth24Plus.

## Possible upstream improvements

These changes could simplify the library. They have not been submitted upstream.

| Project | Opportunity | Evidence |
| --- | --- | --- |
| naga | Parenthesize the bounds-check conditional in the MSL writer. `p[i].x * 0.5` compiles to `cond ? p.inner[i].x : DefaultConstructible() * 0.5`, which Metal rejects. The same class was fixed for swizzles in [naga#83](https://github.com/gogpu/naga/issues/83); [naga#95](https://github.com/gogpu/naga/issues/95) reports it for `select()` only. | `writeLoad`, [expressions.go#L1150-L1175](https://github.com/gogpu/naga/blob/v0.19.0/msl/internal/codegen/expressions.go#L1150-L1175); `TestDerivedUVShader` here |
| wgpu | Fix `examples/triangle-headless` by adding the RenderAttachment to CopySrc transition, or make the copy preflight accept an attachment whose pass has ended. | Ran on Metal, fails at `Finish` |
| wgpu | Populate browser `AdapterInfo` from `GPUAdapterInfo`, including the fallback flag. It reports `"WebGPU Adapter"`, backend `Empty`, type `Other` for every adapter, so a caller cannot detect a software adapter in the browser. | [instance_browser.go#L65](https://github.com/gogpu/wgpu/blob/v0.34.5/instance_browser.go#L65) |
| wgpu | Implement browser error scopes, so creation failures can be reported synchronously. | [device_browser.go#L317-L327](https://github.com/gogpu/wgpu/blob/v0.34.5/device_browser.go#L317-L327) |
| wgpu | A texture readback helper: aligned copy, cancellable map, de-pad. `encodeReadback` and `mapImage` are a working reference for native and browser. | `readback.go` |
| wgpu | Remove a buffer from the pending-map tracker when `Unmap` cancels its map. The abandoned registration is otherwise resolved together with the next `MapAsync` on the same buffer, which then reports `map canceled`. Ran on Metal: `TestRenderCancelledDuringReadback` failed this way until the library replaced the staging buffer after a cancelled wait. | [buffer_mapping.go#L410-L430](https://github.com/gogpu/wgpu/blob/v0.34.5/core/buffer_mapping.go#L410-L430), [device_poll.go#L122-L182](https://github.com/gogpu/wgpu/blob/v0.34.5/core/device_poll.go#L122-L182) |
| wgpu | Capture the waiter before starting the goroutine in native `MapPending.Wait`. A cancelled `Wait` followed by `Release` clears the field that goroutine reads. Found by reading, not reproduced. This library polls to completion before `Wait`, so it takes the path that starts no goroutine. | [map_pending.go#L90-L118](https://github.com/gogpu/wgpu/blob/v0.34.5/map_pending.go#L90-L118) |
| wgpu | Omit stencil operations in the browser for formats with no stencil aspect, instead of relying on `StencilReadOnly`. | [convert_resources.go#L808-L815](https://github.com/gogpu/wgpu/blob/v0.34.5/internal/browser/convert_resources.go#L808-L815) |
| wgpu | Track texture writes and sampling in the device tracker. `Queue.WriteTexture` leaves a texture in the shader-read layout without telling the tracker, and bind groups record no usage, so a later copy from that texture runs in the wrong Vulkan layout unless the caller declares the transition. Ran on Vulkan under the validation layer. | [pending_writes.go#L299-L338](https://github.com/gogpu/wgpu/blob/v0.34.5/pending_writes.go#L299-L338), [track/texture.go#L229-L266](https://github.com/gogpu/wgpu/blob/v0.34.5/core/track/texture.go#L229-L266) |
| wgpu | Record both usages when a render pass and a copy share an encoder. The scope keeps only the last one, so the tracker moves the attachment to CopySrc before the pass, and a pass that loads the attachment finds the wrong Vulkan layout. Ran on Vulkan under the validation layer. | [encoder_native.go#L218-L222](https://github.com/gogpu/wgpu/blob/v0.34.5/encoder_native.go#L218-L222) |
| wgpu | Honour `TextureDescriptor.ViewFormats` on Metal (set `MTLTextureUsagePixelFormatView`) and DX12 (create a typeless resource). Vulkan sets `MUTABLE_FORMAT`; the other two ignore the list. With it, a drawn `SRGB` texture could be read back through an `RGBA8Unorm` view instead of an sRGB decode and re-encode. | [metal/conv.go#L103-L121](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/metal/conv.go#L103-L121), [dx12/device.go#L1288-L1296](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/dx12/device.go#L1290-L1297) |
| wgpu | Remove a released texture from the device tracker when its tracker index is freed. The next texture given the index otherwise inherits the old usage, and its first submission on Vulkan begins with a barrier from the wrong layout. Ran on Vulkan under the validation layer; see VALIDATION.md. | [texture_native.go#L67-L75](https://github.com/gogpu/wgpu/blob/v0.34.5/texture_native.go#L67-L75), [device_tracker.go#L106-L110](https://github.com/gogpu/wgpu/blob/v0.34.5/core/device_tracker.go#L106-L110) |
| wgpu | Find why `vkMapMemory` intermittently returns a null pointer for a new staging buffer on the NVIDIA GB10, about one `New` in ten across the suite. | [device.go#L421](https://github.com/gogpu/wgpu/blob/v0.34.5/hal/vulkan/device.go#L421) |
| wgpu | Enable `GL_FRAMEBUFFER_SRGB` on desktop GL when an sRGB attachment is bound, so drawing into `RGBA8UnormSrgb` encodes as on other backends. | No reference found in `hal/gles` |
| gogpu | Submit the frame encoder in `RenderToImage`, so custom passes are not dropped on hardware backends. | Ran on Metal |
| gg | Move `internal/gpu` to the wgpu v0.34 struct-parameter API. | Build failure above |

## Not verified

- Runtime behavior on DX12 and GLES, and in browsers other than Chrome.
  Backend coverage for those comes from reading source. Vulkan was run on
  one NVIDIA adapter; see [VALIDATION.md](VALIDATION.md).
- A draw callback that never uses `Frame.Color`. The transitions assume the
  callback renders to it.
- gogpu's headless readback in a browser, and its dropped custom pass on
  backends other than Metal.
- Any gg interoperation against wgpu v0.34.x, since it does not build.
- Whether dropping `FreeCommandBuffer` is safe on DX12.
- That Metal's default limit of uncommitted command buffers is 64. The
  number is Apple's, not upstream's; `TestRenderRecoversAfterManyDrawErrors`
  covers the behavior with 70 failed draws.
- The `MapPending.Wait` hazard listed above, which was read, not reproduced.
- Upstream issue coverage, which was searched but not exhaustively.
