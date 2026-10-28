//go:build js && wasm

// Command web exposes the shared two-pass demo to the browser.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"sync"
	"syscall/js"

	"nimbleterminal.dev/ntgpuimage"
	"nimbleterminal.dev/ntgpuimage/internal/twopass"
)

func main() {
	var mu sync.Mutex
	var renderer *ntgpuimage.Renderer
	render := js.FuncOf(func(_ js.Value, args []js.Value) any {
		// Capture values before returning to JavaScript. GPU calls must run in a
		// goroutine so WebGPU's promises can complete on the browser event loop.
		opts := twopass.Options{Frequency: args[0].Float(), Tiles: args[1].Int(), Resolution: args[2].Int()}
		executor := js.FuncOf(func(_ js.Value, callbacks []js.Value) any {
			resolve, reject := callbacks[0], callbacks[1]
			go func() {
				mu.Lock()
				defer mu.Unlock()
				fail := func(err error) { reject.Invoke(js.Global().Get("Error").New(err.Error())) }
				ctx := context.Background()
				if renderer == nil {
					var err error
					renderer, err = ntgpuimage.New(ctx, ntgpuimage.Options{Width: 512, Height: 512})
					if err != nil {
						fail(err)
						return
					}
				}
				final, texture, err := twopass.Render(ctx, renderer, opts)
				if err != nil {
					fail(err)
					return
				}
				finalURL, err := pngURL(final)
				if err != nil {
					fail(err)
					return
				}
				textureURL, err := pngURL(texture)
				if err != nil {
					fail(err)
					return
				}
				resolve.Invoke(js.ValueOf(map[string]any{"final": finalURL, "texture": textureURL}))
			}()
			return nil
		})
		promise := js.Global().Get("Promise").New(executor)
		executor.Release()
		return promise
	})
	defer render.Release()
	js.Global().Set("ntgpuimageRender", render)
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("ntgpuimage-ready"))
	select {}
}

func pngURL(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
