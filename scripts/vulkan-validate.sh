#!/bin/sh
# Runs the GPU tests on Vulkan under the Khronos validation layer, and fails
# on any validation error. A passing test cannot show a wrong image layout;
# the validation layer can.
#
#   sh scripts/vulkan-validate.sh [go test flags]
#
# The layer comes from the system if the vulkan-validationlayers package is
# installed. Otherwise the script unpacks that package, without installing
# it, under GPUIMAGE_VK_LAYER_ROOT, which defaults to
# ~/.cache/go-gpuimage/vulkan-validationlayers. That needs apt-get and dpkg.
set -eu

manifest=VkLayer_khronos_validation.json
system=
for dir in /usr/share/vulkan/explicit_layer.d /etc/vulkan/explicit_layer.d /usr/local/share/vulkan/explicit_layer.d; do
	if [ -f "$dir/$manifest" ]; then
		system=$dir
	fi
done

if [ -z "$system" ]; then
	root=${GPUIMAGE_VK_LAYER_ROOT:-$HOME/.cache/go-gpuimage/vulkan-validationlayers}
	layers=$root/usr/share/vulkan/explicit_layer.d
	if [ ! -f "$layers/$manifest" ]; then
		if ! command -v apt-get >/dev/null || ! command -v dpkg >/dev/null; then
			echo "vulkan-validate: install the Vulkan validation layer, or unpack it under $root" >&2
			exit 2
		fi
		echo "vulkan-validate: unpacking vulkan-validationlayers under $root"
		mkdir -p "$root"
		(cd "$root" && apt-get download vulkan-validationlayers >/dev/null && dpkg -x vulkan-validationlayers_*.deb . && rm -f vulkan-validationlayers_*.deb)
	fi
	library=$(find "$root" -name 'libVkLayer_khronos_validation.so' | head -n 1)
	if [ ! -f "$layers/$manifest" ] || [ -z "$library" ]; then
		echo "vulkan-validate: no validation layer under $root" >&2
		exit 2
	fi
	export VK_ADD_LAYER_PATH="$layers"
	export LD_LIBRARY_PATH="$(dirname "$library")${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
fi

export VK_INSTANCE_LAYERS=VK_LAYER_KHRONOS_validation
export VK_LOADER_DEBUG=layer # logs the layers it loads, which is checked below
export GPUIMAGE_GPU_TEST=1

log=$(mktemp)
trap 'rm -f "$log"' EXIT
status=0
go test -count=1 -v "$@" . >"$log" 2>&1 || status=$?
grep -E '^(--- (FAIL|SKIP)|ok|FAIL|PASS)|adapter:' "$log" || true

if ! grep -q 'VK_LAYER_KHRONOS_validation' "$log"; then
	echo "vulkan-validate: the validation layer was not loaded" >&2
	exit 1
fi
if [ $# -eq 0 ] && ! grep -q 'backend=Vulkan' "$log"; then
	echo "vulkan-validate: the tests did not run on Vulkan" >&2
	exit 1
fi
errors=$(grep -c 'Validation Error' "$log" || true)
echo "Vulkan validation errors: $errors"
if [ "$errors" -gt 0 ]; then
	grep -A 2 'Validation Error' "$log" | cut -c 1-300
	exit 1
fi
exit "$status"
