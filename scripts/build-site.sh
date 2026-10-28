#!/bin/sh
# Build a self-contained static directory suitable for GitHub project Pages.
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin/site
cp web/index.html web/style.css web/app.js bin/site/
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" bin/site/wasm_exec.js
GOOS=js GOARCH=wasm go build -trimpath -o bin/site/demo.wasm ./examples/web
touch bin/site/.nojekyll
printf 'Built GitHub Pages site in bin/site/\n'
