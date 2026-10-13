//go:build !(js && wasm)

package ntgpuimage

import (
	"context"

	"github.com/gogpu/wgpu"
)

// waitMapped waits on the executor thread for a pending map to resolve.
func waitMapped(ctx context.Context, dev *wgpu.Device, pending *wgpu.MapPending) error {
	if ctx.Done() == nil {
		// Nothing can end the wait early, so block until the GPU is idle.
		// That resolves the map, and Wait returns at once.
		dev.Poll(wgpu.PollWait)
		return pending.Wait(ctx)
	}
	// Device.Poll(PollWait) cannot be interrupted, so poll without blocking
	// and watch ctx between attempts.
	return pollUntil(ctx, func() (bool, error) {
		dev.Poll(wgpu.PollPoll)
		return pending.Status()
	})
}
