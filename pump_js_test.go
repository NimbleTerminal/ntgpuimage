//go:build js && wasm

package gpuimage

import (
	"syscall/js"
	"testing"
)

func TestPumpCancelsBeforeRelease(t *testing.T) {
	global := js.Global()
	oldRAF, oldCancel := global.Get("requestAnimationFrame"), global.Get("cancelAnimationFrame")
	defer global.Set("requestAnimationFrame", oldRAF)
	defer global.Set("cancelAnimationFrame", oldCancel)
	var callback js.Value
	next, cancelled := 0, 0
	raf := js.FuncOf(func(_ js.Value, args []js.Value) any { next++; callback = args[0]; return next })
	defer raf.Release()
	cancel := js.FuncOf(func(_ js.Value, args []js.Value) any {
		cancelled = args[0].Int()
		callback = js.Undefined()
		return nil
	})
	defer cancel.Release()
	global.Set("requestAnimationFrame", raf)
	global.Set("cancelAnimationFrame", cancel)
	var p framePump
	p.start()
	callback.Invoke()
	p.stop()
	p.stop()
	if next != 2 || cancelled != 2 || !callback.IsUndefined() {
		t.Fatal("queued animation frame survived stop")
	}
}
