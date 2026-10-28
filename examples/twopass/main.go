// Twopass draws a procedural pattern into a GPU texture with one shader,
// then samples that texture in a second shader to fill the main target,
// all in one frame and without reading pixels back in between. It writes the
// final frame and the intermediate texture as PNG files.
package main

import (
	"context"
	"flag"
	"image"
	"image/png"
	"log"
	"os"

	"nimbleterminal.dev/ntgpuimage"
	"nimbleterminal.dev/ntgpuimage/internal/twopass"
)

func main() {
	out := flag.String("out", "twopass.png", "PNG path for the final frame")
	texOut := flag.String("texture", "twopass-texture.png", "PNG path for the intermediate texture")
	flag.Parse()
	if err := run(*out, *texOut); err != nil {
		log.Fatal(err)
	}
}

func run(out, texOut string) error {
	ctx := context.Background()
	r, err := ntgpuimage.New(ctx, ntgpuimage.Options{Width: 256, Height: 256})
	if err != nil {
		return err
	}
	defer r.Close()

	img, texImg, err := twopass.Render(ctx, r, twopass.Options{Frequency: 60, Tiles: 2, Resolution: 128})
	if err != nil {
		return err
	}
	if err := writePNG(out, img); err != nil {
		return err
	}
	return writePNG(texOut, texImg)
}

func writePNG(path string, img image.Image) error {
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
