# Source filesystem for the guestbench guest root: Debian userland plus the Rust
# toolchain. build.sh exports this image into an ext4 image; it is never run.
FROM rust:1.91-slim-bookworm

# Exactly what the runner shells out to: git, rg, fio, mount.nfs, ip, ps and
# friends, tar, and CA certificates for crates.io. netbase holds the
# /etc/protocols and /etc/rpc that mount.nfs resolves "tcp" and its programs in.
# erofs-utils is here so the host can build the tree's EROFS image by running
# mkfs.erofs from this image; kmod loads the ovnfs kernel module.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git ripgrep fio nfs-common netbase iproute2 procps util-linux tar \
        ca-certificates erofs-utils kmod \
    && rm -rf /var/lib/apt/lists/*

# The workload trees are owned by the host user, so git must trust every path.
RUN git config --system safe.directory '*'

# /bench is the virtio-fs mount point, /mnt/t the NFS mount point, /work the
# scratch directory for local workloads.
RUN mkdir -p /bench /mnt/t /work

COPY init /init
RUN chmod 0755 /init
