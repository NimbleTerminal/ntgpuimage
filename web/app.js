/* The page only controls Go/WASM and displays its PNG readbacks. */
"use strict";
const $ = (id) => document.getElementById(id);
const form = $("controls");
const state = $("state");
const download = $("download");
let revision = 0;
let rendering = false;
let timer;
let ready = false;

function showError(message) {
  $("error").textContent = message;
  $("error").hidden = false;
  state.textContent = "Demo unavailable";
  $("previews").setAttribute("aria-busy", "false");
}

function updateLabels() {
  $("frequency-value").value = $("frequency").value;
  $("tiles-value").value = $("tiles").value;
}

function scheduleRender() {
  updateLabels();
  revision++;
  clearTimeout(timer);
  // A previous download should never silently export different settings.
  download.removeAttribute("href");
  download.setAttribute("aria-disabled", "true");
  state.textContent = "Rendering…";
  timer = setTimeout(renderLatest, 180);
}

async function renderLatest() {
  if (!ready || rendering) return;
  rendering = true;
  const current = revision;
  const frequency = Number($("frequency").value);
  const tiles = Number($("tiles").value);
  const resolution = Number($("resolution").value);
  $("previews").setAttribute("aria-busy", "true");
  const start = performance.now();
  try {
    const result = await globalThis.ntgpuimageRender(frequency, tiles, resolution);
    // Inputs may change while WebGPU completes a readback. Only publish the
    // latest request; the finally block starts it once this one finishes.
    if (current !== revision) return;
    const texture = new Image();
    const final = new Image();
    texture.src = result.texture;
    final.src = result.final;
    await Promise.all([texture.decode(), final.decode()]);
    if (current !== revision) return;
    $("texture").src = result.texture;
    $("final").src = result.final;
    $("texture").hidden = false;
    $("final").hidden = false;
    $("texture-size").textContent = `${resolution} × ${resolution}`;
    download.href = result.final;
    download.setAttribute("aria-disabled", "false");
    $("error").hidden = true;
    state.textContent = "WebGPU · ready";
    $("render-info").textContent = `${Math.round(performance.now() - start)} ms · render + readback + PNG · 512 × 512 RGBA`;
  } catch (error) {
    if (current === revision) showError(`Rendering failed: ${error.message || error}. Reload to retry, or check that WebGPU is available in this browser.`);
  } finally {
    rendering = false;
    $("previews").setAttribute("aria-busy", "false");
    if (current !== revision) renderLatest();
  }
}

form.addEventListener("submit", (event) => event.preventDefault());
form.addEventListener("input", scheduleRender);
form.addEventListener("reset", () => setTimeout(scheduleRender, 0));

async function boot() {
  try {
    if (!globalThis.isSecureContext || !navigator.gpu) {
      throw new Error("This demo needs WebGPU in a secure browser context. Open it over HTTPS (or localhost) in a browser with WebGPU enabled.");
    }
    const adapter = await navigator.gpu.requestAdapter();
    if (!adapter) throw new Error("No WebGPU adapter is available. Check your browser's graphics settings and GPU support.");
    const go = new Go();
    const wasmReady = new Promise((resolve) => addEventListener("ntgpuimage-ready", resolve, {once: true}));
    const response = await fetch(new URL("./demo.wasm", location.href));
    if (!response.ok) throw new Error(`Unable to load the Go demo (HTTP ${response.status}).`);
    // ArrayBuffer instantiation also works on static servers without a WASM MIME type.
    const {instance} = await WebAssembly.instantiate(await response.arrayBuffer(), go.importObject);
    go.run(instance).then(() => {
      ready = false;
      form.querySelector("fieldset").disabled = true;
      showError("The Go renderer stopped. Reload the page to restart it.");
    }).catch((error) => {
      ready = false;
      form.querySelector("fieldset").disabled = true;
      showError(`The Go renderer stopped: ${error.message || error}`);
    });
    await wasmReady;
    ready = true;
    form.querySelector("fieldset").disabled = false;
    await renderLatest();
  } catch (error) {
    showError(error.message || String(error));
  }
}
boot();
