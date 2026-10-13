//go:build js && wasm

package ntgpuimage

import (
	"context"

	"github.com/gogpu/wgpu"
)

// waitMapped awaits the mapAsync promise. Chrome only settles it when the
// page ticks, so a frame pump keeps requestAnimationFrame running until the
// map lands or ctx ends.
func waitMapped(ctx context.Context, _ *wgpu.Device, pending *wgpu.MapPending) error {
	var pump framePump
	pump.start()
	defer pump.stop()
	return pending.Wait(ctx)
}
