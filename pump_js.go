//go:build js && wasm

package ntgpuimage

import "syscall/js"

// framePump keeps the page ticking with requestAnimationFrame while a
// buffer map is pending. Chrome delivers WebGPU mapAsync completions when
// the page's device ticks, which happens on animation frames and other
// GPU activity; a static terminal page may not tick for many tens of
// milliseconds. Requesting frames helps the browser complete the map
// promptly, even when the page is otherwise idle.
type framePump struct {
	fn      js.Func
	running bool
	request js.Value
}

// start begins requesting animation frames until stop is called. It is a
// no-op when requestAnimationFrame is unavailable (workers, tests).
func (p *framePump) start() {
	raf := js.Global().Get("requestAnimationFrame")
	if raf.Type() != js.TypeFunction || js.Global().Get("cancelAnimationFrame").Type() != js.TypeFunction {
		return
	}
	p.running = true
	p.fn = js.FuncOf(func(js.Value, []js.Value) any {
		if p.running {
			p.request = js.Global().Call("requestAnimationFrame", p.fn)
		}
		return nil
	})
	p.request = js.Global().Call("requestAnimationFrame", p.fn)
}

// stop cancels the queued callback before releasing its Go function.
func (p *framePump) stop() {
	if !p.running {
		return
	}
	p.running = false
	if cancel := js.Global().Get("cancelAnimationFrame"); cancel.Type() == js.TypeFunction {
		cancel.Invoke(p.request)
	}
	p.fn.Release()
}
