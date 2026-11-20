// Package wire is the contract between guestbench on the host and its runner
// in the guest.
//
// The host writes one [Plan] as plan.json into a read-only virtio-fs share
// (tag [Tag]) beside the runner binary. The guest's /init mounts the share at
// [Dir] and execs the runner, which reports every timed phase as [Event]
// lines on the console and then powers the VM off.
package wire

import "time"

// Layout of the share the host hands the guest.
const (
	// Tag is the virtio-fs tag of the share holding the runner and its plan.
	Tag = "bench"
	// Dir is where the guest's /init mounts the share.
	Dir = "/bench"
	// Runner is the runner binary, relative to Dir.
	Runner = "guest"
	// PlanFile is the JSON-encoded Plan, relative to Dir.
	PlanFile = "plan.json"
	// FixtureTar is the fixture's work tree without .git as an uncompressed
	// tar, relative to Dir; only KindLocal plans have it.
	FixtureTar = "fixture.tar"
	// GitTar is the contents of the fixture's git directory as an
	// uncompressed tar, relative to Dir. A checkout's own .git is a file
	// naming a host path, so the runner keeps the git directory on its root
	// disk for every kind.
	GitTar = "git.tar"
	// ModuleFile is the ovnfs kernel module, relative to Dir; only KindModule
	// plans have it.
	ModuleFile = "ovnfs.ko"
)

// Guest paths the runner owns.
const (
	// Mount is where the runner mounts the tree under test.
	Mount = "/mnt/t"
	// Local is where the runner unpacks FixtureTar for KindLocal.
	Local = "/work/t"
	// GitDir is where the runner unpacks GitTar; git runs with it as its git
	// directory and the tree as its work tree.
	GitDir = "/work/git"
	// Lower is where the runner mounts a KindImage plan's EROFS image.
	Lower = "/mnt/lower"
	// Overlay holds the upper and work directories of a KindImage plan's
	// overlayfs, on the root filesystem.
	Overlay = "/work/overlay"
)

// Prefix starts every console line carrying an Event; its JSON follows on the
// same line.
const Prefix = "GUESTBENCH "

// Settle is how long the runner idles after printing a begin or end event, so
// the host can sample the daemon's counters between phases.
const Settle = 150 * time.Millisecond

// Kind is how the guest reaches the tree under test.
type Kind string

// Kinds of trees.
const (
	// KindVirtiofs mounts a virtio-fs tag, the VMM's remote-fs v1 proxy device.
	KindVirtiofs Kind = "virtiofs"
	// KindNFS mounts the daemon's NFS port through the VM's network.
	KindNFS Kind = "nfs"
	// KindLocal unpacks the fixture on the root disk: ext4 on virtio-blk, the
	// ceiling the other kinds are compared with.
	KindLocal Kind = "local"
	// KindImage mounts an EROFS image of the tree, stored on the root disk
	// behind the root filesystem, as the read-only lower of an overlayfs whose
	// upper is a directory of the root filesystem. Nothing but block reads of
	// the image leaves the guest.
	KindImage Kind = "image"
	// KindModule loads ModuleFile and mounts filesystem type "ovnfs": the
	// kernel client, which traps into the VMM for every uncached operation.
	KindModule Kind = "module"
)

// Plan tells the runner what to mount and which workloads to time.
type Plan struct {
	// Config names the column the results report under, e.g. "nfs4".
	Config string `json:"config"`
	// Kind selects how Source is mounted.
	Kind Kind `json:"kind"`
	// Source is the mount source: a virtio-fs tag or "host:/export"; empty for KindLocal.
	Source string `json:"source,omitempty"`
	// Options is the mount option string.
	Options string `json:"options,omitempty"`
	// Subdir is the tree's root below Mount; empty when the mount root is the tree.
	Subdir string `json:"subdir,omitempty"`
	// Image locates the tree's EROFS image; set exactly for KindImage.
	Image *Image `json:"image,omitempty"`
	// Net configures the guest's network; nil leaves it down.
	Net *Network `json:"net,omitempty"`
	// Echo is the host:port of the calibration server as the guest dials it.
	// The runner also sets its clock from it, since the VM boots at the Unix
	// epoch; empty skips both that and the net workload.
	Echo string `json:"echo,omitempty"`
	// Workloads lists the workloads to run, each one of [Workloads].
	Workloads []string `json:"workloads"`
	// Rounds is how often every workload runs.
	Rounds int `json:"rounds"`
	// Colds is how many leading rounds run cargo.cold.
	Colds int `json:"colds"`
	// Jobs is the parallelism of the small and replay workloads.
	Jobs int `json:"jobs"`
	// Sources lists the crate-relative source files a replayed no-op build stats.
	Sources []string `json:"sources"`
	// Touch is the crate-relative source touched before incremental phases.
	Touch string `json:"touch"`
}

// Image is the byte range of a guest block device that holds an EROFS image.
type Image struct {
	// Device is the block device, e.g. "/dev/vda".
	Device string `json:"device"`
	// Offset is where the image starts, in bytes; a multiple of 512.
	Offset int64 `json:"offset"`
	// Size is the image's length in bytes; a multiple of 512.
	Size int64 `json:"size"`
}

// Network is the guest's static network configuration.
type Network struct {
	// Addr is the guest's address in CIDR form, e.g. "10.0.2.15/24".
	Addr string `json:"addr"`
	// Gateway is the default route, e.g. "10.0.2.2".
	Gateway string `json:"gateway"`
}

// Event kinds.
const (
	// EvBoot is the runner's first line, before it mounts anything.
	EvBoot = "boot"
	// EvBegin precedes a timed phase; the phase starts Settle later.
	EvBegin = "begin"
	// EvEnd follows a timed phase and carries its measurements.
	EvEnd = "end"
	// EvFatal reports the error that ends the run; no EvDone follows.
	EvFatal = "fatal"
	// EvDone is the runner's last line before it powers the VM off.
	EvDone = "done"
)

// Event is one console report of the runner.
type Event struct {
	// Ev is the event kind, one of the Ev constants.
	Ev string `json:"ev"`
	// Phase names the timed phase, e.g. "walk.cold"; set on begin and end.
	Phase string `json:"phase,omitempty"`
	// Round is the zero-based round of the phase.
	Round int `json:"round"`
	// NS is the phase's duration in nanoseconds.
	NS int64 `json:"ns,omitempty"`
	// Ops counts the phase's file operations or round trips, when it has a natural count.
	Ops int64 `json:"ops,omitempty"`
	// Bytes counts the payload bytes the phase moved, for throughput phases.
	Bytes int64 `json:"bytes,omitempty"`
	// Extra holds phase-specific measurements, e.g. fio's "iops" and "p99_us".
	Extra map[string]float64 `json:"extra,omitempty"`
	// Msg is the error of a fatal event, or a note.
	Msg string `json:"msg,omitempty"`
}

// Workload names.
const (
	// Net calibrates the VM's network against the host's echo server.
	Net = "net"
	// Trap measures one uncached round trip to whatever serves the tree: a
	// statfs of the tree's root, which no client answers from a cache.
	Trap = "trap"
	// Walk lstats every entry of the tree.
	Walk = "walk"
	// Git runs `git status --porcelain` in the tree.
	Git = "git"
	// Rg searches every file of the tree with ripgrep.
	Rg = "rg"
	// Fio measures buffered sequential and random I/O on one large file.
	Fio = "fio"
	// Small creates, stats, renames and unlinks many small files.
	Small = "small"
	// Replay replays cargo's filesystem traffic without compilers.
	Replay = "replay"
	// Cargo runs `cargo build --offline` of the fixture's crate.
	Cargo = "cargo"
	// Verify checks what the tree stores instead of timing it for its own
	// sake: every tracked file against git's index, and written data against
	// what was written after the guest's caches are dropped. A mismatch is
	// fatal, so no result comes from a filesystem that returns wrong bytes.
	Verify = "verify"
)

// Workloads lists every workload in the order the runner runs them.
var Workloads = []string{Net, Trap, Walk, Git, Rg, Fio, Small, Replay, Cargo, Verify}

// Phases lists each workload's phases in run order. A ".cold" phase starts
// with the tree remounted (except KindLocal) and the guest's caches dropped.
var Phases = map[string][]string{
	Net:    {"net.rr", "net.up", "net.down"},
	Trap:   {"trap.one", "trap.par"},
	Walk:   {"walk.cold", "walk.warm"},
	Git:    {"git.cold", "git.warm"},
	Rg:     {"rg.cold", "rg.warm"},
	Fio:    {"fio.seq-write", "fio.seq-read", "fio.rand-write", "fio.rand-read"},
	Small:  {"small.create", "small.sync", "small.stat", "small.rename", "small.unlink", "small.settle"},
	Replay: {"replay.cold", "replay.no-op", "replay.touch", "replay.clean"},
	Cargo:  {"cargo.cold", "cargo.no-op", "cargo.touch"},
	Verify: {"verify.tree", "verify.data", "verify.churn"},
}

// Modes of the calibration server, sent as the first byte of a connection.
const (
	// EchoRR makes the server echo every further byte until EOF.
	EchoRR byte = 'r'
	// EchoUp makes the server discard everything until EOF, then write the
	// discarded byte count as 8 little-endian bytes and close.
	EchoUp byte = 'u'
	// EchoDown is followed by an 8-byte little-endian length; the server
	// writes that many zero bytes and closes.
	EchoDown byte = 'd'
	// EchoTime makes the server write its wall clock as 8 little-endian bytes
	// of Unix nanoseconds and close.
	EchoTime byte = 't'
)
