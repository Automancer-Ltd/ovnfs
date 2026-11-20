#!/usr/bin/env bash
# Builds the guestbench guest root filesystem as a sparse ext4 image. Only
# docker is needed on the host: the image is built, exported, unpacked as root
# and formatted with mke2fs inside containers, so it runs on macOS and Linux.
set -euo pipefail

usage() {
	cat >&2 <<USAGE
usage: build.sh [--arch aarch64|x86_64] [--out FILE] [--size SIZE]

  --arch   guest architecture (default: host architecture)
  --out    ext4 image to write (default: \${XDG_CACHE_HOME:-\$HOME/.cache}/ovnfs-guestbench/rootfs-<arch>.ext4)
  --size   image size, mke2fs syntax such as 8G (default: 8G)
USAGE
	exit 2
}

die() {
	echo "build.sh: $*" >&2
	exit 1
}

# host_arch maps the host's uname to the architecture names of --arch.
host_arch() {
	case "$(uname -m)" in
	arm64 | aarch64) echo aarch64 ;;
	x86_64 | amd64) echo x86_64 ;;
	*) die "unsupported host architecture $(uname -m); pass --arch" ;;
	esac
}

arch=""
out=""
size="8G"
while [[ $# -gt 0 ]]; do
	case "$1" in
	--arch) [[ $# -ge 2 ]] || usage; arch="$2"; shift 2 ;;
	--out) [[ $# -ge 2 ]] || usage; out="$2"; shift 2 ;;
	--size) [[ $# -ge 2 ]] || usage; size="$2"; shift 2 ;;
	-h | --help) usage ;;
	*) usage ;;
	esac
done

command -v docker >/dev/null 2>&1 ||
	die "docker not found in PATH; install docker, it is the only host requirement"
docker info >/dev/null 2>&1 ||
	die "cannot reach the docker daemon; start docker and retry"

[[ -n "$arch" ]] || arch="$(host_arch)"
case "$arch" in
aarch64) platform="linux/arm64" ;;
x86_64) platform="linux/amd64" ;;
*) die "unknown --arch '$arch'; want aarch64 or x86_64" ;;
esac
[[ "$size" =~ ^[0-9]+[KkMmGgTt]?$ ]] || die "invalid --size '$size'; want e.g. 8G"
[[ -n "$out" ]] || out="${XDG_CACHE_HOME:-$HOME/.cache}/ovnfs-guestbench/rootfs-$arch.ext4"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
mkdir -p "$(dirname "$out")"
out_dir="$(cd "$(dirname "$out")" && pwd -P)"
out="$out_dir/$(basename "$out")"
tmp="$out.tmp"
image="ovnfs-guestbench-rootfs:$arch"
mkfs_image="ovnfs-guestbench-mkfs:1"
container="ovnfs-guestbench-rootfs-$arch-$$"

# cleanup removes the export container and any partial image on every exit.
cleanup() {
	docker rm -f "$container" >/dev/null 2>&1 || true
	rm -f "$tmp"
}
trap cleanup EXIT

echo "building $image ($platform)" >&2
docker build --platform "$platform" -t "$image" "$here" >&2

# The formatter image is host-native: mke2fs output does not depend on the
# guest architecture, so no emulation is needed.
docker build -t "$mkfs_image" - >&2 <<'DOCKERFILE'
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends e2fsprogs tar \
    && rm -rf /var/lib/apt/lists/*
DOCKERFILE

# The image is formatted in the container's own filesystem and copied out by
# seeking over its zero blocks: macOS bind mounts refuse the hole punching of
# `cp --sparse=always`, and seeking still leaves the host file sparse.
echo "writing $out ($size)" >&2
docker create --platform "$platform" --name "$container" "$image" /init >/dev/null
docker export "$container" |
	docker run --rm -i -v "$out_dir:/out" \
		-e "SIZE=$size" -e "NAME=$(basename "$tmp")" "$mkfs_image" sh -ec '
	mkdir /rootfs
	tar -xpf - --numeric-owner --same-owner -C /rootfs
	mke2fs -q -t ext4 -d /rootfs -L root -m 0 -E lazy_itable_init=1 /tmp/root.ext4 "$SIZE"
	dd if=/tmp/root.ext4 of="/out/$NAME" bs=1M conv=sparse status=none
'

mv -f "$tmp" "$out"
echo "$out"
