# Changelog

## Unreleased

Initial version:

- Offscreen WebGPU rendering on native platforms and in the browser.
- A persistent RGBA8 target with optional depth and resize support.
- Custom shaders and pipelines through upstream WebGPU types.
- Image upload, sampling, updates, and readback as `image.NRGBA`.
- GPU work serialized on one OS thread, with `Do` for setup and cleanup.
- Context cancellation while waiting to draw or read pixels.
- Device-limit checks before allocating targets or copying image pixels.
- Frame timing statistics and adapter information.
- Examples for clearing a target and sampling an uploaded image.
- Native, browser, and Vulkan validation tasks.
