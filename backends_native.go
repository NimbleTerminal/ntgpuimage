//go:build !(js && wasm)

package ntgpuimage

// Register every native HAL backend (Metal, Vulkan, DX12, GLES, software).
// The browser build has no HAL layer and must not see this import.
import _ "github.com/gogpu/wgpu/hal/allbackends"
