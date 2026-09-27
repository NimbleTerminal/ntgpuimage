package gpuimage

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"os"
	"testing"

	"github.com/gogpu/gputypes"
)

// BenchmarkRenderReadback measures a complete frame: clear the color target,
// submit, wait for the GPU, and copy pixels into a new image.NRGBA. Device
// creation, warmup, context creation, and cleanup are outside the timer.
func BenchmarkRenderReadback(b *testing.B) {
	if os.Getenv("GPUIMAGE_GPU_TEST") != "1" {
		b.Skip("set GPUIMAGE_GPU_TEST=1 to run GPU benchmarks")
	}
	var backends gputypes.Backends
	switch backend := os.Getenv("GPUIMAGE_BENCH_BACKEND"); backend {
	case "":
	case "metal":
		backends = gputypes.BackendsMetal
	case "vulkan":
		backends = gputypes.BackendsVulkan
	default:
		b.Fatalf("unsupported GPUIMAGE_BENCH_BACKEND %q; use metal or vulkan", backend)
	}
	for _, size := range []struct{ width, height int }{{640, 400}, {1280, 720}, {1920, 1080}} {
		b.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(b *testing.B) {
			for _, cancellable := range []bool{false, true} {
				name := "Background"
				if cancellable {
					name = "Cancellable"
				}
				b.Run(name, func(b *testing.B) {
					r, err := New(context.Background(), Options{
						Width: size.width, Height: size.height, Backends: backends,
					})
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() { _ = r.Close() })
					info := r.AdapterInfo()
					b.Logf("adapter: %q backend=%v type=%v", info.Name, info.Backend, info.DeviceType)
					if info.DeviceType == gputypes.DeviceTypeCPU {
						b.Fatal("GPU benchmark selected a software adapter")
					}
					ctx := context.Background()
					if cancellable {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						defer cancel()
					}
					draw := clearTo(gputypes.Color{R: 1, A: 1})
					var img *image.NRGBA
					for range 10 {
						img, _, err = r.Render(ctx, draw)
						if err != nil {
							b.Fatal(err)
						}
					}
					b.ReportAllocs()
					b.SetBytes(int64(size.width) * int64(size.height) * 4)
					for b.Loop() {
						img, _, err = r.Render(ctx, draw)
						if err != nil {
							b.Fatal(err)
						}
					}
					// Check the last frame outside the timed loop.
					if img.Bounds() != image.Rect(0, 0, size.width, size.height) ||
						img.NRGBAAt(size.width/2, size.height/2) != (color.NRGBA{R: 255, A: 255}) {
						b.Fatal("unexpected readback image")
					}
				})
			}
		})
	}
}
