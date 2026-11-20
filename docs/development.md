# Development

```sh
go build -o bin/ovnfs .
go test ./...
```

`go test ./...` runs the package and in-process protocol checks; on Windows
with WinFsp installed it also mounts real WinFsp drives. The git engines under
`git/internal` are checked against the `git` binary when one is installed
(fixtures and expected output come from it); those tests skip without it.
They are written from git's documentation and observed behaviour, never from
git's or another implementation's source.

## Overlay and kernel-share tests

The overlay is checked against a reference model: an in-memory POSIX tree
(`overlay/model_test.go`) that knows nothing of layers, uppers, memory files
or indexes, except where the overlay deliberately differs from POSIX, each
such rule citing its source. `overlay/model_run_test.go` drives seeded random
operation streams through the overlay and the model and fails on the first
difference in a result class, a handle's contents or the whole view.
`TestModelSequential` runs one stream over a lower stack with whiteouts, an
opaque directory and hard links, in three memory configurations: `memory=off`,
`memory=spill` (an 8 KiB budget, a 2 KiB per-file limit and a 1 ms idle age, so
files spill by size, by eviction and by background write-back all through the
run) and `memory=roomy` (room for everything, so most files never reach the
disk before `Flush`). At random points, and once more at the end, it takes
four checkpoints: a reopen of the flushed upper without memory, a cool (the
index unloads and the tree must read the same cold), a freeze (the flushed
upper becomes a layer and a fresh upper continues on top), and a crash (the
upper as it is on disk, with host changes still queued and files still in
memory, is copied and an overlay opened on the copy must show every name in a
state it had since the last `Flush`; names are ordered, contents are not).
`TestModelConcurrent` runs four streams at once, each in its own subtree and
its own names of one shared directory, with a model per stream, and compares
the merged models with the final tree before and after `Flush` and a reopen.
`TestModelDeniedOpenKeepsIdentity` checks that a refused write-open of one name
of a frozen read-only hard link changes nothing, so the name keeps its
identity. A failure prints the seed, the environment that reproduces it and
the last operations.

| variable | effect |
| --- | --- |
| `OVERLAY_MODEL_SEED` | the stream's seed (default 1) |
| `OVERLAY_MODEL_OPS` | operations per sequential stream (default 1500); raise it for soak runs |
| `OVERLAY_MODEL_FINDINGS` | known findings to reproduce instead of leaving out: a comma-separated list or `all` |
| `OVERLAY_MODEL_HISTORY` | operations a failure report shows (default 40) |

Two findings, where the overlay differs from the model and no document or test
justifies the overlay, stay excluded unless asked for: `readdir-below-file`
(`ReadDir` below a regular file only the bottom provides fails with ENOTDIR,
while `Lstat` of that path reports it missing) and `root-mtime` (reopening a
non-empty upper and freezing it move the root's mtime to the present).

The host side of the memory tier has its own tests.
`TestQueueCrashLeavesAnEarlierState` queues renames, links, unlinks, mkdirs
and rmdirs without letting the workers make them, lets them make one change at
a time, and after each opens an overlay on a copy of the upper as a crash would
leave it, which must show every name in a state it had.
`TestQueueParksAFailure` checks that a change the host refuses parks the queue,
which the budget reports and `Flush` returns, while new changes go to the host
themselves, and that the queue moves on once the host accepts it.
`TestHostOpsConcurrent` runs workers that create, spill, read, rename, link and
remove files while the write-back worker moves idle files to disk and another
worker renames a directory whose files spill meanwhile; every file must read
back as written, and after `Flush` the index must match the upper on disk,
directory times included. `TestMemorySyscallTrace` records an execution trace
of each memory-tier name change and of the same changes on files on disk, so
`go tool trace -d=parsed` can count the host syscalls each makes; it runs only
with `OVERLAY_SYSCALL_TRACE` naming a directory for the traces.

`kfs`, the kernel client's server, is tested by protocol: `TestCompound`,
`TestCompoundMalformed` and `TestCompoundReplyLimit` for COMPOUND,
`TestCreateWithGuestIDs` for guest-chosen ids, and `TestOpsByIDAgainstRenames`,
which runs operations by id on one connection while another renames the file
back and forth, renames its directory back and forth and renames other files
onto its old name, for a file in the memory tier and one on disk; no operation
by id may fail, none may touch the files renamed onto the old name, and the
writes must end up in the file. Three measurements run only on request, against
a daemon's kernel share or an in-process fixture:

- `TestMutationWorkload` (`KFS_WORKLOAD`) drives the guest benchmark's
  small-file workload over four connections: 10,000 1 KiB files in 100
  directories created and written, read back after the memory tier let them go,
  linked, renamed and removed, and logs each operation's count, mean, median and
  daemon file calls. `KFS_WORKLOAD_SOCK` names a daemon's share (its checkout
  must hold an empty directory named by `KFS_WORKLOAD_DIR`, default `w`),
  `KFS_WORKLOAD_WORKERS` the connections (default 4).
- `TestCompoundCreates` (`KFS_WORKLOAD`) measures what one connection's
  COMPOUNDs of 256 requests cost the daemon per file: creates with a guest id
  and 4 KiB of content in one CREATE, the same as CREATE, WRITE and SETATTR, and
  renames and unlinks of the files while the memory tier holds them, 20,000
  files each. `KFS_WORKLOAD_MEMORY` sets the in-process fixture's budget in MiB.
- `TestRoundTrip` (`KFS_RTT`, `KFS_RTT_SOCK`, `KFS_RTT_CONNS`) measures GETATTR
  round trips, 20,000 per connection, and logs their median and p99.

## CLI

The command line is a [kong](https://github.com/alecthomas/kong) grammar: one
struct per verb — the root in [`grammar.go`](../grammar.go), each verb's flags
and help beside its implementation, all in struct tags. Help sections are kong
groups, git-style flags take `short:` tags, and values like `--idle 7d` parse
through `encoding.TextUnmarshaler`. [`ui.go`](../ui.go) colours the output, and
`colorHelp` renders kong's help in the same palette.

[kong-completion](https://github.com/jotaen/kong-completion) generates
initialization code for bash, zsh and fish (`ovnfs completion bash -c`): verbs,
a verb's own flags and their enums come from the grammar, checkout and snapshot
names from a daemon that is already running (completion never starts one, see
`predictCheckouts`). The global options stay out of the grammar, so they are
listed in help but not completed.

`run` and `serve` are commands in the same grammar. `splitGlobals` pulls the
global options (`--state`, `--mount`, `--json`, `--color`) out of the arguments
before the `--` separator, so they work before the verb or among its arguments.
The separator preserves the arguments of a command launched by `run`.

## CI and releases

[`ci.yml`](../.github/workflows/ci.yml) runs on pushes to `main` and on pull
requests: `gofmt`, `go mod tidy -diff`, `go vet` for Linux, macOS and Windows,
a snapshot GoReleaser build, then `go test` on all three (race detector on
Linux and macOS, real FUSE mounts on Linux, real WinFsp drives on Windows).

Pushing a `v*` tag runs the same checks, then
[`release.yml`](../.github/workflows/release.yml) has GoReleaser
([`.goreleaser.yaml`](../.goreleaser.yaml)) publish a GitHub release with
archives for Linux, macOS and Windows on amd64 and arm64, and
[`internal/release/upload`](../internal/release/upload/main.go) publish the
same binaries to Stencil's build service, `build.stencil.so`, which takes the
run's GitHub OIDC token as proof of the tag:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

Go stamps the tag into the build info, so `ovnfs version` reports it for release
archives and `go install …@v0.2.0` alike, and a VCS pseudo-version in a clone.

[`install.sh`](../install.sh) and [`install.ps1`](../install.ps1), which
`https://stencil.so/ovnfs.sh` and `/ovnfs.ps1` redirect to on `main`, `update`
and `send` download release binaries from the build service and check their
SHA-256 there; `OVNFS_BUILD_URL` points all of them, and the upload, at another
origin. The service hosts `ovnfs` because its product table
(`apps/build/products.go` in the Stencil repository) lets `stencil-hq/ovnfs`
publish `v<version>` tags.

## Benchmark

### Quick I/O comparison

`ovnfs bench [DIR]` compares the active ovnfs mount with a native disk
directory, normally in about 10 seconds. Without DIR, disk scratch files live
in the daemon's state directory, on the same volume as its backing files.
DIR selects another existing directory; a directory inside the ovnfs mount
is rejected. The daemon starts automatically if needed.

```sh
ovnfs bench
ovnfs bench /path/on/another/disk
ovnfs --json bench
```

Each side gets a 32 MiB file of identical pseudorandom data. Four workloads
run for 750 ms per side: sequential reads and writes in 1 MiB blocks, then
random reads and writes in 4 KiB blocks. Operations are single-threaded,
queue depth one, with the same random-offset seed and alternating which
filesystem runs first. Writes include a final `fsync` in their timing.

Output reports MiB/s, IOPS, and ovnfs throughput divided by disk throughput
(higher is better). The normal CLI palette highlights workloads and ratios;
`--color` and `NO_COLOR` control it as for other commands.
These are **buffered filesystem measurements**, not raw
device speeds: OS caches stay enabled, and reads use newly written files
rather than cold source-layer reads. The working set stays at 32 MiB per
side, though each sample may rewrite it many times. JSON includes completed
bytes, operations, actual sample durations, and the same rates and ratios.

Setup and I/O have a 23-second budget, with up to three seconds for checkout
removal. I/O runs in a killable child so a stalled mount does not hold up the
command. Timeout and interruption report an error and still attempt cleanup;
failed checkout removal prints the `ovnfs rm` command to retry. Successful
runs remove the temporary checkout and scratch files, leaving any daemon
they started running.

### Balanced configuration tuning

`ovnfs tune` compares disposable daemons and mounts on the backing state's
volume. It leaves the live daemon and user configuration alone during trials:

```sh
ovnfs tune
ovnfs tune --project /path/to/cargo/workspace
ovnfs tune --proto nfs --runs 1
ovnfs --json tune
```

The search always includes the configured `serve` baseline, then the selected
platform frontends, NFS transfer sizes of 16, 64, 128, 256 and 1024 KiB, and
memory disabled versus the configured budget. Cargo trials also consider 256 MiB.
Environment overrides constrain the search so an applied result is not silently
overridden on restart. Existing fsync, atime and security policies are retained;
NFS is excluded when switching to it would remove explicitly enabled fsync
forwarding.

Each configuration runs the same four I/O workloads as `bench`, artifact
creation/write/rename/publication/removal, and directory/stat/fingerprint
reads. Artifact publication uses real hard links or a byte-copy fallback where
unsupported, reports which happened, and verifies the resulting bytes.
`--project` adds real Cargo cold, no-op and touched-source incremental builds
in disposable checkouts. Compiler wrappers are disabled, the original
workspace's toolchain is retained, and Cargo runs locked/offline without
automatic toolchain installation. Build scripts still execute normally.
Existing target directories are hidden; the source checkout is not touched.
Source snapshot identities must agree across trials or tuning aborts rather
than comparing different code.

The default is three samples (`--runs`); Cargo cold builds run once per
configuration and warm phases use the sample count. `--timeout` bounds each
configuration (default 2 min); native mount commands retain their 5 s deadlines.
Workers and compiler children are killed on cancellation. Cleanup has a separate
12 s budget, may force-detach only private trial mounts, and retains a failed
cleanup's directory with its location rather than traversing a stalled mount.

Ranking compares each workload with the configured baseline, normalizing I/O
and metadata against their paired native measurements. By default, any workload
regression exceeding 20% disqualifies a candidate (`--max-regression`).
Gains are capped at 2x for scoring, the I/O/metadata/Cargo groups have equal
weight, and the weakest workload also contributes half the score in log space.
A candidate must beat the baseline score by 3%; ties keep the baseline.
Thus a spectacular random-write win cannot conceal a slow metadata or Cargo
path. This is the best measured configuration, not a guarantee for every workload.

After reporting, the command asks `Apply? [y/N]`. Only `y` or `yes` activates
the selected protocol, memory budget and NFS size, restarts the selected daemon,
then atomically saves those three keys in `~/.o2/ovnfs.yml`, preserving unrelated
settings and comments. Failed activation leaves that file unchanged. Concurrent
edits to tuned keys are not overwritten. Protocol or option changes may force
a remount and invalidate active checkout handles; finish those jobs first.
EOF declines; `--json` reports only and never prompts or applies.

### Build comparisons

`bench.sh REPO [TOUCH]` compares Cargo build times across ways of getting a
second working tree of REPO: a git worktree, an ovnfs checkout over each
protocol (NFS and SMB on macOS; FUSE, NFS and SMB on Linux, mounting the latter
two through passwordless sudo), and a copy-on-write clone. It times create,
snapshot, fork, cold, no-op and incremental builds; TOUCH (default
`src/main.rs`) is the file touched for incremental rows.

Results go to `BENCH_DIR/results.tsv`, by default `ovnfs-bench` beside REPO so
the clone and the daemon's state share its volume. `BUILD` (default
`cargo build`), `OVNFS` (default `bin/ovnfs`) and `PROTOS` override the rest;
run BUILD without cross-tree caches such as sccache, which would hide the
filesystem's effect. `bench.sh --clean REPO` removes `BENCH_DIR` and its
worktree.

`scripts/buildbench` compares ovnfs binaries on build-shaped IO and memory.
Each `-ovnfs` binary gets a private daemon (temporary state, mount and HOME,
so default settings) and a checkout of one fixture repository: the
`-project`'s files (or a synthetic crate) plus `-files` bulk files (default
100,000) for a realistically large base, committed with one `git fast-import`
stream in about five seconds. Every round runs the same work in a host
directory and in each checkout, rotating the order: `io` (the default)
replays the filesystem traffic of a cold `cargo build` of a 149-package
workspace (82 rlibs, build scripts, fingerprints, 256 codegen objects, a 24 MB
linked binary hard-linked into place), a no-op build, a relink after a touch,
and a clean; `cargo` runs `cargo build --offline` cold in the first round and
no-op and after a touch in every round. It prints each phase's median, its
ratio to the host and its range, the capture time, and each daemon's
footprint (macOS) or RSS (Linux), its peak and the cache use from
`ovnfs status`, after capture and after the work (and after `-idle`, if set).
The default run, two binaries and three `io` rounds, takes about 25 seconds:

```sh
(cd "$(ovnfs new --name old --rev main --json | jq -r .checkout.path)" && go build -o /tmp/ovnfs-old .)
go build -o /tmp/ovnfs-new .
go run ./scripts/buildbench -ovnfs /tmp/ovnfs-old -ovnfs /tmp/ovnfs-new -project ~/src/some-crate -modes io,cargo -runs 5
```

### Guest comparisons

`scripts/guestbench` times the same workloads on one tree as a vibevmm guest
reaches it four ways: `remotefs` (the VMM's remote-fs v1 virtio-fs device on
an `ovnfs share` socket), `nfs3` and `nfs4` (the guest kernel's NFS client on
the daemon's TCP port, through the VM's user-mode NAT) and `ext4` (the fixture
unpacked on the guest's root disk, the ceiling the others are compared with).
It starts a private daemon built from the working tree, opens one checkout of
buildbench's fixture per config and boots one VM for each. Its runner,
cross-built into a read-only share, is the guest's PID 1: it sets the clock
from the host (a guest boots at the Unix epoch), mounts the tree and reports
every phase on the console.

The workloads are `net` (round-trip time and throughput of the VM's network,
the floor under the NFS columns), `trap` (`trap.one` and `trap.par`: one
uncached round trip to whatever serves the tree, a loop of `statfs` calls on
its root that no client answers from a cache, with one worker and with one per
job), `walk` (an lstat of every entry), `git` (`git status`, with the git
directory on the guest's disk, since a checkout's `.git` names a host path),
`rg`, `fio`, `small` (create, stat, rename and unlink 10,000 files, with a
`syncfs` after the creates, `small.sync`, and after the unlinks,
`small.settle`, so work a filesystem defers shows on every column), `replay`
(buildbench's `io` phases), `cargo` and `verify` (correctness; see below). A
`.cold` phase remounts the tree and drops the guest's caches first. At every
phase boundary the daemon's `calls` timers are sampled, so the report shows,
beside each median and its ratio to ext4, the daemon calls per guest operation
and their summed duration; `-out` saves every event.

It needs vibevmm's `vmon` and guest kernel (`just release` and
`just kernel --arch aarch64` in a vibevmm checkout) and a guest image from
`scripts/guestbench/rootfs/build.sh`, which needs only docker. It runs on
macOS hosts: vmon's user-mode network, which carries NFS and the calibration,
exists only there. The default run, 50,000 bulk files and three rounds, takes
about 40 minutes, most of it in the remote-fs column:

```sh
scripts/guestbench/rootfs/build.sh
go run ./scripts/guestbench -vmon ~/src/vibevmm/target/release/vmon \
    -kernel ~/src/vibevmm/target/kernel/Image-aarch64 \
    -rootfs ~/.cache/ovnfs-guestbench/rootfs-aarch64.ext4 -out results.json
```

`verify` checks instead of timing. `verify.tree` hashes every tracked file and
compares it with git's blob id, and `verify.data` runs sixteen written-data
cases (page and megabyte boundaries, back-to-front and mid-file rewrites,
truncation up and down, a shared writable mapping, rename and hard links),
each read back after a cold reset, so the bytes come from the filesystem and
not the guest's caches. `verify.churn` runs four workers at once, each in its
own subtree, through 4,000 seeded random operations apiece: create (written
three ways), write, truncate, rename of files and directories, hard link,
unlink, mkdir, rmdir, symlink, chmod, set-mtime and read, on five names so
that operations collide often. Every operation's outcome (success or the exact
error) is checked against a model of what POSIX requires, and every 200
operations and after the last the whole subtree is compared with the model:
each directory's names, and each entry's type, permission bits, symlink
target, and a file's size, link count, mtime and content. The mtime checks are
exact against the value `stat` showed after the file's last change, and stamped
times must fall within the run's wall-clock span plus a slack. After the
workers finish, the tree is compared again after a cold reset, and once more
after `syncfs`, a two-second wait and another cold reset, so changes a
filesystem applies late are caught. A failure names the worker and its seed
(`churnSeed` in `guest/churn.go`), which replays the sequence.

Two more configs are opt-in (`-configs kmod,erofs,nfs3,ext4`):

- `kmod` reaches the tree through the kernel client: the daemon serves the
  checkout with `ovnfs share NAME --rw --kernel`, vmon gets `--ovnfs SOCK`,
  and the runner loads `ovnfs.ko` and mounts filesystem type `ovnfs`.
  `kmod/build.sh` builds the module in docker against vibevmm's pinned guest
  kernel (it needs the `vmon-kernel-build` image and the `vmon-kernel-cache`
  volume that `just kernel` leaves behind) and checks its vermagic; `-ko FILE`
  takes a prebuilt one. At the end of a run the module's own counters
  (`/sys/kernel/debug/ovnfs/calls`: calls and nanoseconds per operation, the
  COMPOUND and ALLOC_IDS calls included, then the journal's counters, see
  [kernel-client.md](kernel-client.md#counters)) and the guest kernel's
  warnings are saved with the results.
- `erofs` gives the guest an EROFS image of the tree as the lower of an
  overlayfs whose upper is the guest's disk, so nothing but block reads leaves
  the guest. The image is built with `mkfs.erofs` from the guest image's
  docker image (`-mkfs-image`) and appended to the VM's private root disk.

The numbers of the first run and the targets derived from them are in
[kernel-client.md](kernel-client.md).
