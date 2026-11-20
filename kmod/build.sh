#!/usr/bin/env bash
# Builds ovnfs.ko, the ovnfs guest filesystem, as an external module against
# the vibevmm guest kernel, inside vibevmm's kernel build container.
#
# Usage: kmod/build.sh [--arch aarch64|x86_64] [--out FILE] [--vibevmm DIR]
#   --arch     guest architecture (default aarch64)
#   --out      where to put the module (default kmod/ovnfs.ko)
#   --vibevmm  vibevmm checkout whose kernel/build.sh pins the kernel (default /work/vibevmm)
#
# The kernel source and build tree are the ones vibevmm's `just kernel` leaves
# in the `vmon-kernel-cache` Docker volume; the toolchain is the
# `vmon-kernel-build` image. Fails on compiler warnings (W=1) and when the
# module's vermagic does not name the tree's kernel release. Prints the
# module's path as the last line.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
arch=aarch64
out="$here/ovnfs.ko"
vibevmm=/work/vibevmm
image=vmon-kernel-build
volume=vmon-kernel-cache

usage() {
	sed -n '5,8p' "${BASH_SOURCE[0]}" | sed 's/^# //'
	exit "$1"
}

die() {
	echo "build.sh: $*" >&2
	exit 1
}

while (($#)); do
	case "$1" in
	--arch) arch="$2"; shift 2 ;;
	--out) out="$2"; shift 2 ;;
	--vibevmm) vibevmm="$2"; shift 2 ;;
	-h | --help) usage 0 ;;
	*) echo "build.sh: unknown argument: $1" >&2; usage 2 ;;
	esac
done

case "$arch" in
aarch64) karch=arm64 ;;
x86_64) karch=x86 ;;
*) die "unsupported arch '$arch' (aarch64|x86_64)" ;;
esac

kernel_build="$vibevmm/kernel/build.sh"
[[ -f "$kernel_build" ]] || die "no $kernel_build (pass --vibevmm DIR)"
tag="$(sed -n 's/^KERNEL_TAG=//p' "$kernel_build")"
timestamp="$(sed -n 's/^BUILD_TIMESTAMP="\(.*\)"$/\1/p' "$kernel_build")"
[[ -n "$tag" && -n "$timestamp" ]] || die "cannot read KERNEL_TAG/BUILD_TIMESTAMP from $kernel_build"

docker image inspect "$image" >/dev/null 2>&1 ||
	docker build --quiet --tag "$image" "$vibevmm/kernel" >/dev/null

# Runs in the container with this directory at /kmod.
read -r -d '' inner <<'EOF' || true
set -euo pipefail
src="/root/.cache/vmon-kernel/src/linux-$TAG"
build="/root/.cache/vmon-kernel/build/$TAG-$ARCH"
[[ -f "$build/.config" && -f "$build/vmlinux.o" ]] ||
	{ echo "build.sh: no kernel build tree at $build (run vibevmm's just kernel --arch $ARCH)" >&2; exit 1; }
cross=""
[[ "$(uname -m)" == "$ARCH" ]] || cross="$ARCH-linux-gnu-"
export KBUILD_BUILD_TIMESTAMP="$TIMESTAMP" KBUILD_BUILD_USER=vmon KBUILD_BUILD_HOST=vmon KBUILD_BUILD_VERSION=1
args=(-C "$src" O="$build" ARCH="$KARCH" CROSS_COMPILE="$cross" -j"$(nproc)")
if [[ ! -f "$build/Module.symvers" ]]; then
	# The kernel builds no modules; this only runs modpost over vmlinux to
	# write the Module.symvers external modules link against.
	echo "==> writing $build/Module.symvers" >&2
	make "${args[@]}" -s modules >&2
fi
make "${args[@]}" M=/kmod clean >/dev/null
log="$(mktemp)"
make "${args[@]}" M=/kmod W=1 modules >"$log" 2>&1 || { cat "$log" >&2; exit 1; }
cat "$log" >&2
if grep -iE 'warning:|error:' "$log" >/dev/null; then
	echo "build.sh: the module does not compile cleanly" >&2
	exit 1
fi
info="$(readelf -p .modinfo /kmod/ovnfs.ko | sed -n 's/^ *\[ *[0-9a-f]*\] *//p')"
echo "$info" >&2
vermagic="$(echo "$info" | sed -n 's/^vermagic=//p')"
release="$(cat "$build/include/config/kernel.release")"
[[ "${vermagic%% *}" == "$release" ]] ||
	{ echo "build.sh: vermagic '$vermagic' does not match kernel release '$release'" >&2; exit 1; }
echo "$info" | grep -qx 'license=GPL' || { echo "build.sh: module license is not GPL" >&2; exit 1; }
EOF

docker run --rm --cpus 4 \
	--volume "$here:/kmod" --volume "$volume:/root/.cache/vmon-kernel" \
	--env TAG="$tag" --env ARCH="$arch" --env KARCH="$karch" --env TIMESTAMP="$timestamp" \
	"$image" nice -n 10 bash -c "$inner"

if [[ "$out" != "$here/ovnfs.ko" ]]; then
	mkdir -p "$(dirname "$out")"
	cp "$here/ovnfs.ko" "$out"
fi
echo "$(cd "$(dirname "$out")" && pwd)/$(basename "$out")"
