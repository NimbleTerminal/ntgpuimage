// Clear renders a solid color offscreen and writes the pixels as a PNG.
package main

import (
	"context"
	"flag"
	"image/png"
	"log"
	"os"

	"github.com/gogpu/gputypes"
	"github.com/gogpu/wgpu"
	"nimbleterminal.dev/ntgpuimage"
)

func main() {
	out := flag.String("out", "frame.png", "output PNG path")
	flag.Parse()
	if err := run(*out); err != nil {
		log.Fatal(err)
	}
}

func run(path string) error {
	ctx := context.Background()
	r, err := ntgpuimage.New(ctx, ntgpuimage.Options{Width: 640, Height: 480})
	if err != nil {
		return err
	}
	defer r.Close()
	img, _, err := r.Render(ctx, func(f *ntgpuimage.Frame) error {
		pass, err := f.Encoder.BeginRenderPass(&wgpu.RenderPassDescriptor{
			ColorAttachments: []wgpu.RenderPassColorAttachment{{
				View: f.Color, LoadOp: gputypes.LoadOpClear, StoreOp: gputypes.StoreOpStore,
				ClearValue: gputypes.Color{R: 0.1, G: 0.4, B: 0.7, A: 1},
			}},
		})
		if err != nil {
			return err
		}
		return pass.End()
	})
	if err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(file, img); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
