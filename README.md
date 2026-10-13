# ntgpuimage

[`ntgpuimage`](https://github.com/NimbleTerminal/ntgpuimage) is an offscreen WebGPU rendering for Golang, built on
[gogpu/wgpu](https://github.com/gogpu/wgpu). The Go package is `ntgpuimage`, imported as `nimbleterminal.dev/ntgpuimage`.

Render into a GPU texture with your own shaders, upload and sample Go
images, and read pixels back as `*image.NRGBA`. There is no window and no
event loop. The same API builds natively and for browser WebAssembly
(`GOOS=js GOARCH=wasm`).

The library manages the render target, texture resources, GPU execution, and
readback. Shaders, pipelines, buffers, and drawing use upstream WebGPU types
directly.

The current release is v0.2.0. The API may change before v1.0.
[DESIGN.md](DESIGN.md) explains the design and compares related GoGPU libraries.
[VALIDATION.md](VALIDATION.md) records what was tested and how.

## Quickstart

Install the library:

```sh
go get nimbleterminal.dev/ntgpuimage@v0.2.0
```

```go
import (
	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"

	"nimbleterminal.dev/ntgpuimage"
)

r, err := ntgpuimage.New(ctx, ntgpuimage.Options{Width: 640, Height: 480, Depth: true})
if err != nil { /* errors.Is(err, ntgpuimage.ErrNoAdapter), or device creation failed */ }
defer r.Close()

// Create pipelines, shaders, and buffers on the renderer's GPU thread.
err = r.Do(func(dev *wgpu.Device) error { /* dev.CreateRenderPipeline(...) */ return nil })

img, stats, err := r.Render(ctx, func(f *ntgpuimage.Frame) error {
	// f.Device.Queue().WriteBuffer(...) updates uniforms for this frame.
	pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
		ColorAttachments: []wgpu.RenderPassColorAttachment{{
			View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore,
			ClearValue: gputypes.Color{R: 0.1, G: 0.1, B: 0.1, A: 1},
		}},
		DepthStencilAttachment: &wgpu.RenderPassDepthStencilAttachment{
			View: f.Depth, DepthLoadOp: gputypes.LoadOpClear, DepthStoreOp: gputypes.StoreOpStore,
			DepthClearValue: 1, StencilReadOnly: true, // required in the browser
		},
	})
	if err != nil { return err }
	// pass.SetPipeline(...); pass.Draw(...)
	return pass.End()
})
// img is a new *image.NRGBA that the caller owns.
```

Run the examples:

```sh
go run ./examples/clear -out frame.png      # clear a target and read it back
go run ./examples/texture -out texture.png  # upload, sample, update, read back
go run ./examples/twopass -out twopass.png  # render into a texture, then sample it
```

## API

| Call | Purpose |
| --- | --- |
| `New(ctx, Options)` | Create the device and the first target. |
| `Renderer.Do(func(*wgpu.Device) error)` | Run setup or teardown on the GPU thread. |
| `Renderer.Render(ctx, draw)` | Draw, then read the frame back in the same submission. |
| `Renderer.RenderToTexture(ctx, draw)` | Draw without reading back. |
| `Renderer.ReadPixels(ctx)` | Read back the most recent frame. |
| `Renderer.Resize(w, h)`, `Size()` | Replace the target. A failed resize keeps the old one. |
| `Renderer.AdapterInfo()` | Adapter name, backend, and device type. |
| `Renderer.UploadTexture(image, TextureOptions)` | Create a sampled texture from a Go image. |
| `Renderer.NewTexture(w, h, TextureOptions)` | Create a blank texture, typically a `RenderTarget`. |
| `Frame.RenderTo(texture, draw)` | Draw into a `RenderTarget` texture within the current frame. |
| `Texture.Update`, `ReadPixels`, `Size`, `Close` | Replace pixels in place, copy them back, release. |
| `Texture.BindGroup()`, `BindGroupLayout()` | Handles for the caller's pipeline. |
| `Renderer.Close()` | Release everything, including textures still open. |

`Frame` gives the draw callback `Device`, `Encoder`, `Color`, `Depth`,
`Width`, `Height`, and `Seq`. `Stats` reports `Draw`, `Submit`, `Map`,
`Copy`, and `Total` durations for a frame.

## Pixels and alpha

- Targets and textures are `RGBA8Unorm`. Colors are not converted between
  color spaces.
- Readback returns the target's bytes as `image.NRGBA`. If a shader writes
  premultiplied color, un-premultiply before treating the image as straight
  alpha. Opaque output needs no conversion.
- `UploadTexture` accepts any `image.Image`, including sub-images and padded
  strides, and stores straight alpha. Linear filtering of straight alpha
  darkens edges next to transparent texels; use an opaque image, nearest
  filtering, or a padded atlas where that matters.
- GoGPU's `gogpu` and `gg` libraries use premultiplied alpha. Convert when
  exchanging images with them.

## Sampled images

```go
tile, err := r.UploadTexture(sourceImage, ntgpuimage.TextureOptions{})
if err != nil { /* handle error */ }
defer tile.Close()

// Include tile.BindGroupLayout() in your pipeline layout. In WGSL:
// @group(0) @binding(0) var tileTexture: texture_2d<f32>;
// @group(0) @binding(1) var tileSampler: sampler;
// Inside a draw callback, bind tile.BindGroup() at that group index.

err = tile.Update(nextImage)         // same dimensions; the bind group stays valid
pixels, err := tile.ReadPixels(ctx)  // optional CPU copy of the texture
```

`TextureOptions{Filter: gputypes.FilterModeNearest}` selects nearest
filtering. The zero value selects linear. UVs clamp at the edges.

## Rendering into a texture

A texture created with `RenderTarget: true` can be drawn into by one pass
and sampled by a later pass, in the same frame or a later one, without a
CPU readback in between. `Frame.RenderTo` hands the draw callback a `Frame`
whose `Color`, `Depth`, `Width`, and `Height` describe the texture, so the
same drawing code works against either target.

```go
scratch, err := r.NewTexture(512, 512, ntgpuimage.TextureOptions{RenderTarget: true, Depth: true})
if err != nil { /* handle error */ }
defer scratch.Close()

img, _, err := r.Render(ctx, func(f *ntgpuimage.Frame) error {
	// Pass 1: draw into the texture. t.Color and t.Depth are its views.
	if err := f.RenderTo(scratch, func(t *ntgpuimage.Frame) error {
		return drawScene(t) // BeginRenderPass on t.Encoder with t.Color
	}); err != nil {
		return err
	}
	// Pass 2: sample scratch.BindGroup() while drawing into f.Color.
	return drawPostProcess(f, scratch)
})
```

- Attach the views of the nested `Frame`, not the outer one. The nested
  `Frame` is cleared when its callback returns.
- `RenderTo` records the layout transitions the pass needs on Vulkan, and
  `ReadPixels` on a drawn texture goes through a GPU blit rather than a
  direct copy, so both pass the Vulkan validation layer; see
  [VALIDATION.md](./VALIDATION.md). Both are no-ops on Metal and in the
  browser.
- A render target has a fixed size. To change it, create a new texture and
  close the old one. `Renderer.Resize` does not affect textures.
- `Depth: true` allocates a `Depth24Plus` attachment for the texture and
  requires `RenderTarget`. The nested `Frame.Depth` is nil otherwise.
- `UploadTexture` also accepts `RenderTarget`, for drawing over an image.
- `RenderTo` cannot be nested. An error from its callback fails the frame
  even if the outer callback ignores it. Sampling a texture that has never
  been written or drawn reads undefined pixels, and `ReadPixels` on it
  returns an error.

## Threading and ownership

- Each `Renderer` owns its instance, adapter, device, target, and textures,
  and releases them in `Close`. Calls that need the device return
  `ErrClosed` after it closes.
- All GPU work runs on one goroutine pinned to an OS thread. Calls from any
  goroutine are serialized, with one frame in flight.
- Do not call methods that queue GPU work from inside `Do` or a draw
  callback. They would wait for the thread they already occupy. Texture
  handle accessors such as `BindGroup` and `BindGroupLayout` are safe to
  use in callbacks.
- Objects you create with the device are yours. Release them inside `Do`
  before `Close`.
- Do not keep the device, encoder, or views from a callback for use on
  another goroutine.
- Sharing a device owned by another library is not supported yet.
  [DESIGN.md](DESIGN.md#accepting-an-externally-owned-device) describes a
  possible API.

## Cancellation

`Render`, `RenderToTexture`, and both `ReadPixels` methods take a context.

- A call waiting for the GPU thread returns `ctx.Err()` when the context
  is cancelled or expires, without drawing.
- Cancellation during the draw callback or while the GPU finishes the
  frame also returns `ctx.Err()`. A running draw callback is not
  interrupted.
- After cancellation the GPU may still complete the abandoned frame.
  Later calls are unaffected.
- On Metal, a cancellable context added about 0.1 ms per readback in
  testing, because the wait must poll. `context.Background()` uses a
  single blocking wait.
- With a cancellable context, native `RenderToTexture` returns once the
  frame is submitted instead of waiting for the GPU.
- `Do`, `Resize`, `UploadTexture`, `Update`, and `Close` take no context and
  wait their turn.

## Browser

- Requires a WebGPU-capable browser in a secure context (HTTPS or
  localhost).
- Call the library from a Go goroutine, not from inside a JavaScript
  callback. Readback awaits a promise, which would deadlock the event loop.
- While a readback is pending, the library requests animation frames so
  that the browser settles the map promptly.
- Set `StencilReadOnly: true` when attaching `Frame.Depth`. The browser
  backend otherwise emits stencil operations, which WebGPU rejects for a
  depth-only format.
- `AdapterInfo` cannot identify a software adapter in the browser. Upstream
  reports every browser adapter as `"WebGPU Adapter"`.

## Testing

With [Task](https://taskfile.dev) installed, `task list` shows every task.

| Task | Runs | Needs |
| --- | --- | --- |
| `task` | Race tests and builds of examples, the browser runner, and WASM | Nothing |
| `task ci` | Everything CI runs: race tests, vet, WASM build, Node tests | Node |
| `task test-gpu` | The full suite on a native adapter | A GPU |
| `task bench` | Clear and readback benchmarks, three runs per case | A GPU |
| `task test-browser` | The full suite in headless Chrome | Chrome with WebGPU |
| `task validate` | `ci`, `test-gpu`, and `test-browser` | All of the above |
| `task test-vulkan` | The full suite on Vulkan under the validation layer | Linux with Vulkan |
| `task test-vulkan-remote HOST=name` | Copies the tree to `name` over ssh and runs `test-vulkan` there | ssh access |
| `task run-examples` | Both examples, writing PNG files to `bin/` | A GPU |

The same commands without Task:

```sh
go test -race ./...                   # helpers and executor, no GPU
GPUIMAGE_GPU_TEST=1 go test ./...     # real native adapter; never with -race

# Helper tests under Node, which has no WebGPU:
GOOS=js GOARCH=wasm go test -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" .

# The full suite in headless Chrome with WebGPU:
go build -o /tmp/browsertest ./internal/browsertest
GPUIMAGE_GPU_TEST=1 GOOS=js GOARCH=wasm go test -exec=/tmp/browsertest -v .
```

GPU tests log the adapter they ran on. A run on a CPU or fallback adapter
does not test hardware GPU behavior.

## Benchmarks

`BenchmarkRenderReadback` measures one complete frame: clear an RGBA8
color target, submit it, wait for the GPU, and copy the pixels into a new
`image.NRGBA`. It includes allocation and garbage collection during the
loop. Device creation, ten warmup frames, context creation, and cleanup
are outside the timer. It does not draw geometry, upload textures, or use
a depth attachment.

Each size runs with `context.Background()` and with one reused cancellable
context that is never cancelled. This measures the cost of checking for
cancellation, not the cost of cancelling a frame.

```sh
task bench                            # choose the default native backend
task bench BACKEND=metal               # macOS
task bench BACKEND=vulkan              # Linux
task bench BENCHTIME=3s COUNT=5         # longer runs

# Without Task (set GPUIMAGE_BENCH_BACKEND=metal or vulkan to choose):
GPUIMAGE_GPU_TEST=1 go test -run='^$' -bench=BenchmarkRenderReadback \
  -benchmem -benchtime=1s -count=3 .
```

Run without the race detector or Vulkan validation layers. The benchmark
logs the adapter and rejects software adapters. `MB/s` is the output image
size divided by frame time; it is not a measurement of GPU bus bandwidth.
Allocation counts cover Go memory, not GPU memory.

Results from 2026-09-27, using wgpu v0.34.5 and naga v0.19.0. Each value
is the median of three runs with `-benchtime=1s`. Lower is better.

| Frame size | Context | Apple M3 Max / Metal | NVIDIA GB10 / Vulkan |
| --- | --- | --- | --- |
| 640x400 | Background | 0.587 ms | 0.480 ms |
| 640x400 | Cancellable | 0.645 ms | 0.684 ms |
| 1280x720 | Background | 0.657 ms | 0.768 ms |
| 1280x720 | Cancellable | 0.758 ms | 0.938 ms |
| 1920x1080 | Background | 0.820 ms | 1.077 ms |
| 1920x1080 | Cancellable | 0.954 ms | 1.199 ms |

The Metal run used macOS 27.0 arm64 and Go 1.27.1. The Vulkan run used
Ubuntu 24.04.5 arm64, Go 1.26.8, and NVIDIA driver 580.178.04. Vulkan used
`VK_DRIVER_FILES=/usr/share/vulkan/icd.d/nvidia_icd.json` to select the
NVIDIA driver. These numbers compare complete systems, including CPU,
driver, and Go runtime costs.

Raw output, including throughput and allocations:
[Metal](benchmarks/2026-09-27-metal.txt) and
[Vulkan](benchmarks/2026-09-27-vulkan.txt).

## Known limitations

- naga v0.19.0 generates invalid Metal source for arithmetic on a component
  of an indexed `let` or `const` array, such as `positions[i].x * 0.5`.
  List the values explicitly, as the examples do. `TestDerivedUVShader`
  reports when upstream fixes this.
- The main color target cannot be sampled by a later pass. It has no
  `TextureBinding` usage. Draw into a `RenderTarget` texture with
  `Frame.RenderTo` instead.
- Render-target textures have no resize; create a new one.
- No mipmaps, MSAA, sRGB formats, or partial texture updates.
- Metal, Vulkan on one NVIDIA adapter, and Chrome were run. DX12, GLES, and
  other browsers compile but were not tested.
- `ReadPixels` fails until a frame has been drawn to the current target,
  and `Texture.ReadPixels` fails until the texture has been written or
  drawn into.


## Open Collaboration

We welcome contributions and feedback.  Please adhere to our [Code of Conduct](./CODE_OF_CONDUCT.md) when engaging our community.

 * [GitHub Issues](https://github.com/NimbleTerminal/ntgpuimage/issues)
 * [GitHub Pull Requests](https://github.com/NimbleTerminal/ntgpuimage/pulls)

## Acknowledgements

Thanks to the [GoGPU organization](https://gogpu.io) for their dedication to making GPU processing available to Golang developers, especially in the form of the [`wgpu` WebGPU library](https://github.com/gogpu/wgpu).

## License

This project is released under the [MIT License](https://en.wikipedia.org/wiki/MIT_License), see [LICENSE.txt](./LICENSE.txt).

Copyright (c) 2026 [Neomantra Corp](https://www.neomantra.com).   

----
Made with :heart: and :fire: by the team behind [Nimble.Markets](https://nimble.markets).
