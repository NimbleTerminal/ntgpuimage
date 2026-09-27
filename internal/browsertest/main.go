//go:build !(js && wasm)

// Command browsertest runs a GOOS=js GOARCH=wasm test binary in headless
// Chrome so tests can reach a real browser WebGPU device. Node has no
// navigator.gpu, so Go's default wasm exec script only covers helper tests.
//
// Build it for the host, then pass it to go test:
//
//	go build -o /tmp/browsertest ./internal/browsertest
//	GPUIMAGE_GPU_TEST=1 GOOS=js GOARCH=wasm go test -exec=/tmp/browsertest -v .
//
// It prints the browser's WebGPU adapter before the test output. A fallback
// adapter is a software rasterizer and does not establish hardware support.
//
// Set CHROME to a Chrome or Chromium binary when it is not in a standard
// location, and BROWSERTEST_TIMEOUT (a Go duration) to change the two-minute
// limit. GPUIMAGE_* variables are forwarded to the test binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const page = `<!doctype html><meta charset="utf-8"><title>browsertest</title>
<script src="/wasm_exec.js"></script>
<script>
(async () => {
  let queue = Promise.resolve();
  const send = (path, body) => queue = queue.then(() => fetch(path, {method: "POST", body}).catch(() => {}));
  const fail = (e) => { send("/out", "browsertest: " + e + "\n"); send("/exit", "1"); };
  addEventListener("error", (e) => fail(e.message));
  addEventListener("unhandledrejection", (e) => fail(e.reason));
  try {
    const cfg = await (await fetch("/config")).json();
    const adapter = navigator.gpu ? await navigator.gpu.requestAdapter() : null;
    const info = adapter ? adapter.info : null;
    send("/out", "browsertest: WebGPU adapter: " + (info
      ? "vendor=" + info.vendor + " architecture=" + info.architecture + " description=" + JSON.stringify(info.description) + " fallback=" + !!info.isFallbackAdapter
      : "none") + "\n");
    const decoder = new TextDecoder();
    globalThis.fs.writeSync = (fd, bytes) => { send("/out", decoder.decode(bytes)); return bytes.length; };
    const go = new Go();
    go.argv = cfg.argv;
    go.env = cfg.env;
    let code = 0;
    go.exit = (c) => { code = c; };
    const {instance} = await WebAssembly.instantiateStreaming(fetch("/test.wasm"), go.importObject);
    await go.run(instance);
    send("/exit", String(code));
  } catch (e) { fail(e); }
})();
</script>`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: browsertest <test.wasm> [test flags]")
		os.Exit(2)
	}
	code, err := run(os.Args[1], os.Args[2:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "browsertest:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(wasm string, args []string) (int, error) {
	timeout := 2 * time.Minute
	if v := os.Getenv("BROWSERTEST_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("BROWSERTEST_TIMEOUT: %w", err)
		}
		timeout = d
	}
	chrome, err := findChrome()
	if err != nil {
		return 0, err
	}
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return 0, fmt.Errorf("go env GOROOT: %w", err)
	}
	wasmExec := filepath.Join(strings.TrimSpace(string(goroot)), "lib", "wasm", "wasm_exec.js")

	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "GPUIMAGE_") {
			env[k] = v
		}
	}
	config, err := json.Marshal(map[string]any{"argv": append([]string{filepath.Base(wasm)}, args...), "env": env})
	if err != nil {
		return 0, err
	}

	exit := make(chan int, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, page)
	})
	mux.HandleFunc("/wasm_exec.js", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, wasmExec) })
	mux.HandleFunc("/test.wasm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/wasm")
		http.ServeFile(w, r, wasm)
	})
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(config)
	})
	mux.HandleFunc("/out", func(w http.ResponseWriter, r *http.Request) { io.Copy(os.Stdout, r.Body) })
	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		code, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil {
			code = 1
		}
		select {
		case exit <- code:
		default:
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()

	profile, err := os.MkdirTemp("", "browsertest-profile-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(profile)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// localhost is a secure context, which WebGPU requires.
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new", "--enable-unsafe-webgpu", "--enable-features=WebGPU",
		"--no-first-run", "--no-default-browser-check", "--disable-extensions",
		"--user-data-dir="+profile,
		"http://"+strings.Replace(listener.Addr().String(), "127.0.0.1", "localhost", 1)+"/")
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", chrome, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		cmd.Process.Kill()
		<-exited
	}()

	select {
	case code := <-exit:
		return code, nil
	case err := <-exited:
		exited <- err
		return 0, fmt.Errorf("browser exited before the test finished: %v", err)
	case <-ctx.Done():
		return 0, fmt.Errorf("no result after %s", timeout)
	}
}

func findChrome() (string, error) {
	if v := os.Getenv("CHROME"); v != "" {
		return v, nil
	}
	candidates := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"}
	if runtime.GOOS == "darwin" {
		candidates = append([]string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}, candidates...)
	}
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no Chrome or Chromium found; set CHROME")
}
