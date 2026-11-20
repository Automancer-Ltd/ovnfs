# Kernel client

A vibevmm guest used to reach a checkout through the VMM's remote-fs v1
virtio-fs device on an `ovnfs share` socket (JSON, path-addressed, one request
in flight), or through the guest kernel's NFS client on the daemon's NFS port.
The kernel client is a guest filesystem driver (`ovnfs.ko`) that answers from
its own caches whatever it can and, for the rest, traps into the VMM, which
relays one binary, id-addressed request to the daemon and resumes the guest
with the reply in its memory. This document records the decisions, the
measured baseline (M0), the targets, and what the first version reaches.

Status: the first version (v0) runs every benchmark workload in a guest. Its
parts are `kmod/` (the module and `ovnfs_wire.h`, the contract all parts
implement), `kfs/` (the daemon's server, `ovnfs share NAME --rw --kernel`) and
vibevmm's call device (`vmon vmm --ovnfs SOCK`). It is not yet integrated into
vibevmm's agent and node, and only the benchmark has exercised it; see
[Remaining work](#remaining-work). v0 meets all but
three of the targets below (`replay.cold`, `cargo.touch`, `fio.rand-write`);
see [First version (v0)](#first-version-v0).

## Decisions

| decision | choice | why |
| --- | --- | --- |
| Language | C | the guest kernel has `CONFIG_RUST` off and no Rust VFS bindings (see the [audit](kernel-client-audit.md)) |
| Transport | a trap: the guest writes the address of a call block to an MMIO doorbell, and the VMM performs the call on the trapping vCPU thread, relaying one length-prefixed request to the daemon's Unix socket and reading the reply straight into guest memory; one doorbell and one daemon connection per vCPU | no virtio driver, interrupt, wait queue or completion in the guest and no device state in the VMM, so snapshot, restore and fork need nothing; a round trip is one exit instead of an exit, a device-thread wakeup and an interrupt. Guest RAM of restored and forked VMs is mapped private (vibevmm `vmm/src/memory.rs`), so the daemon could not serve the guest directly anyway |
| Addressing | the export's stable file ids, confined by the daemon to the share's checkout; id 1 is always the tree's root | an id follows its file through renames; nothing is addressed by path |
| Server state | none per connection or open file: no open, release or forget; the daemon keeps files open by id only as a cache | the VMM may reconnect at any time; restore and fork need no replay |
| Durability | the daemon never syncs for a write or a create; only FSYNC syncs. A journaled change is applied by the daemon after its syscall returned: `fsync`, `sync`, `syncfs` and unmount wait for it, and a refusal after the fact is reported late (see [Late failures](#late-failures)) | the baseline's NFS creates cost milliseconds each because every one was synced |
| Caching | one epoch per mount, carried by every reply: names, missing names, attributes and file contents cached under an epoch are valid until a reply carries another. A directory is listed whole into a table that answers every later lookup, hit or miss, without a call. A poll once a second while the mount is active bounds how late a change made elsewhere is noticed | round trips are what the baseline shows to be unaffordable; a timer per file is what made NFS's warm walk as slow as its cold one |
| Batching | COMPOUND (op 20): one call carries several requests, which the daemon runs in order as if they had arrived one by one on the connection, continuing past one that fails; each reply has its own error and epoch. A malformed compound (a truncated sub-request, a nested COMPOUND) runs nothing and fails as a whole. A sub-request whose largest possible reply does not fit the rest of the frame is answered EMSGSIZE without running, and the sub-requests after it still run. The write-cache barrier (`Options.Quiesce`) is taken once for the whole compound | a batch of creates, renames or unlinks pays for one round trip; bounding the replies keeps the frame within the call's limit |
| Guest-chosen ids | ALLOC_IDS (op 21) hands the guest a run of consecutive ids, at most the count it asked for, that the daemon gives to nothing else; the daemon saves a mark past them before it replies (`allocateRun` in `overlay/identity.go`), so they stay usable after a restart or an epoch change, until the VM is attached to another tree. CREATE (with the new id, an mtime and up to `max_io` bytes of content, which needs `CREATE_EXCL`) and MKDIR (with the new id and an mtime) then create the entry under that id, mtime and content in one step. The daemon checks that the id belongs to the share's checkout, that it is one the overlay's allocator issued and that no visible entry holds it; otherwise EINVAL, and the id is spent either way. It does not check which client was given the id, so one handed out for anything else (a memory file since removed) passes. ALLOC_IDS on a read-only share is EROFS | the guest can show a new file or directory with its final id and mtime before the daemon has created it, and what the daemon later shows equals what the guest showed; content in CREATE saves the WRITE and SETATTR a small file would otherwise need |
| Round-trip latency | after a reply the daemon polls the connection for 50 µs before it parks in the runtime's network poller (`kfs/spin_unix.go`); the VMM likewise polls for the reply for 50 µs before it blocks (vibevmm `vmm/src/devices/ovnfs.rs`; metrics `ovnfs.spin_hits`, `ovnfs.send_ns`, `ovnfs.reply_wait_ns`) | a busy guest CPU sends its next request within microseconds, and waking a parked goroutine or thread costs about as much again; a connection that goes quiet stops costing CPU once the window passes |
| Operations by id | the daemon holds one lock per checkout (`namesOf` in `kfs/server.go`), shared by every share and connection of it: exclusive for RENAME, UNLINK and RMDIR, shared for every other request that resolves an id (HELLO, POLL, STATFS and ALLOC_IDS resolve none, and a compound's sub-requests take it one by one) | an operation resolves an id to a path and then acts on the path in separate export calls, and a rename between the two, of the file, of a directory above it or of another file onto its name, would fail it with ENOENT or make it act on another file |
| Journal | create and mkdir of a name known to be absent, unlink, rmdir of a directory known empty, rename, hard link, SETATTR and a closed file's data update the guest's caches and return; one worker per mount sends them in order in COMPOUND batches (see [The journal](#the-journal)) | a mutation then costs a share of a batch, not a round trip, and the daemon still sees the sequence of syscalls the guest made |
| Mount options | none of the module's own: `ro`, `sync` and `dirsync` are the generic ones, and `sync` and `dirsync` (or the same inode flags) make every change synchronous | one way to ask for the v0 behaviour, which the kernel's own tools already know |
| Page cache | plain page-cache operations with order-0 folios; dirty pages of a closed file are queued for the daemon as a journal entry (inside the file's CREATE when it is small), those of an open file are written back by `writepages` | the transport is synchronous, so the netfs library's request machinery buys nothing here; sending a closed file's data through the journal lets the host see it without any sync, in the same batch as the names |
| Delivery | a loadable module built against the pinned guest kernel (`kmod/build.sh`), to be pushed by the vibevmm node and loaded by its guest agent with `finit_module` | driver updates need no template rebuild (precedent: vibevmm's NVIDIA bundle) |
| Source | `kmod/` in this repository with one shared wire header; vibevmm pins it | client and server change together |
| Lifecycle | the vibevmm node creates, forks, shares and removes checkouts per sandbox | decided by the project owner |
| Windows hosts | not supported yet: the call device is built for Linux and macOS hosts | scope |

## The journal

A mutation in a directory the guest knows well does not trap. The syscall
changes the guest's own caches (dentries, directory listings, inodes) exactly
as a successful reply would, appends an entry to the mount's journal and
returns. One worker per mount takes the entries oldest first, packs them into
COMPOUND calls of up to 1 MiB of request, one call at a time, and applies the
replies in order. A file that is created, written, closed and renamed costs
sub-requests of one call, not a round trip per step.

### What is journaled

| entry | when |
| --- | --- |
| CREATE, MKDIR | the name is known to be absent (a current negative dentry) and an id is in the pool |
| UNLINK | the name and the file's link count are current |
| RMDIR | the directory is known to be empty: its listing is complete and has no entries |
| RENAME | both names are current, a moved or replaced file's link count is current and a replaced directory is known to be empty (files and directories move; `RENAME_NOREPLACE` is supported, other flags are EINVAL) |
| LINK | the file's link count is current and it still has a name |
| SETATTR | mode, an explicit mtime or the size (truncation, `O_TRUNC`) of a regular file or directory whose attributes are current |
| DATA | a closed file's dirty pages |

Every entry also needs a directory the guest can predict the daemon's answer
in: its attributes are current and its owner write bit is set (the daemon acts
on the host as the owner of the tree). The ids of new files and directories
come from a pool that ALLOC_IDS fills 8,192 at a time and refills in the
background when fewer than 2,048 remain; a create finding the pool empty is
synchronous, and if the daemon refuses ALLOC_IDS the pool stays off for the
mount, the module logs once that creates are synchronous, and everything else
is unchanged.

Closing a file opened for writing queues its dirty pages as one DATA entry: a
WRITE per contiguous run, then a SETATTR that gives the daemon the guest's
mtime. A file that is closed, wholly in the page cache, not mapped writable
and at most 256 KiB travels inside its own CREATE (content and mtime in the one
sub-request, which consumes its DATA entry); a truncated and rewritten file of
that size has its size SETATTR carry the whole content as one WRITE. Larger
files go in entries of at most 256 KiB, queued again until none of their data
is left. Files still open are written back by `writepages`, as `fsync` does,
in WRITE calls of up to `max_io` on another port. When a file's last name is
removed (unlink or rename over it, link count 1, not open) its queued DATA
entry is cancelled instead of sent; no other entry is cancelled, so an entry
is sent even if a later one undoes it.

The journal is bounded to 131,072 entries and 32 MiB, which a burst of tens of
thousands of small creates fits; a caller that finds it full waits for the
worker.

### What stays synchronous

- SYMLINK and MKNOD, because the protocol cannot give them guest-chosen ids
  yet: only CREATE and MKDIR carry one.
- Anything whose result the guest cannot predict: a dentry or attributes that
  are not current, a parent directory without its owner write bit, a mode with
  setuid, setgid or sticky bits, a change of owner, an empty id pool.
- Everything on a mount or inode with the standard `sync` or `dirsync` flags
  (`-o sync`, `-o dirsync`, `chattr +S` and `+D`): each such call is one trap
  that returns the daemon's result, and with `sync` a close writes the file's
  data back and waits for it.

The module has no mount options of its own.

### Ordering and the barrier

The daemon receives the journal in append order, each entry once: one batch is
in flight at a time and the sub-requests of a batch run in order, so the daemon
sees a sequence the syscalls could have produced one by one. A file has data
in a batch at most once; later data of the same file starts the next batch.
Entries carry the names and ids of the moment, and the daemon resolves ids
atomically against renames, so an entry that names a file below a directory
renamed earlier still finds it.

Data never overtakes its file's CREATE or a size change. A truncation moves the
file's queued DATA entry behind itself, so data written before it cannot
reappear after it; data taken for an entry that a later size change overtook
is given back and queued again behind it; and a read of a page that is not
cached waits while a size change of the file is unanswered, because the daemon
still holds bytes the change will cut. `writepages` is not ordered with the
journal, so it leaves a file with any unanswered entry alone (background
write-back) or waits for the entries (integrity write-back: `fsync`, `sync`),
and the worker sends none of a file's data or SETATTRs while `writepages` for
it runs, since a WRITE in flight on the other port would undo a truncation or
mtime that overtook it.

A call that goes to the daemon and is not itself journaled first waits until
every entry naming one of the inodes it names has been answered:

- LOOKUP and READDIR of a directory wait for the entries that change a name
  in it, so the daemon's answer includes them;
- SETATTR and FSYNC of an inode wait for the entries naming it: its creation,
  data and links;
- a synchronous namespace change (SYMLINK, MKNOD, and CREATE, MKDIR, UNLINK,
  RMDIR, RENAME or LINK when not journaled) waits for every directory and node
  it names, so the daemon finds the names, ids and emptiness the guest saw;
- `sync`, `syncfs`, unmount and remount read-only wait for everything.

Entries elsewhere need no wait. Some calls never wait: GETATTR of an inode
with unanswered entries is not sent at all, since its cached attributes are
what the entries will produce, and a refresh is not applied to it meanwhile; a
READ goes straight out, because pages that are not cached were never dirty (a
dirty page stays cached until written), pages past the zero point of a file
created or truncated here read as zeros without a call, and a pending size
change is the one case that waits, as above; the WRITEs of `writepages` need
only the data rule; READLINK (symlinks are never journaled), STATFS, POLL and
ALLOC_IDS.

What this guarantees: `fsync` of a file sends the journal up to the entries
naming it, writes back the range and then issues one FSYNC, so the file, its
name and its data are on the daemon and synced. `fsync` of a directory issues
one FSYNC after the entries naming it. `sync`, `syncfs`, unmount and remount
read-only return only when the daemon has answered every entry; unmount drains
before the dcache goes, because late failures revisit dentries. They make the
daemon apply the changes, not sync them: only FSYNC syncs.

### Timestamps

The guest's clock stamps the guest's own changes, so what the guest shows is
what the daemon will report. The daemon is told that mtime: in the CREATE for a
new file, and by a SETATTR after the data of a file written later. A
sub-request carries the mtime the guest shows when it is packed, not the one
of the syscall that queued it; an entry that carries data written after later
entries were queued therefore still gives every later sub-request an mtime at
least as recent, and the last one the daemon runs is the guest's. Replies to
journal entries never move a node's times, and their attributes are applied
only when no later entry names the inode (the daemon has not seen that change
yet) and no change of this guest overtook the call; otherwise only the size and
mtime the daemon reports are noted.

### Late failures

A local filesystem reports an error from the call that caused it. Here a
journaled operation has already returned success when the daemon refuses it:
a full disk, a permission the guest could not foresee, a name another frontend
made meanwhile. The refusal cannot reach the caller. This is a semantic
difference from a local filesystem, and the module handles it as follows:

- it logs the refusal (`ovnfs: deferred NAME of ID/NAME failed: errno N`,
  rate-limited) and counts it in `late_failures`;
- it reports the error once through `syncfs` (the superblock's write-back
  error) and through `fsync` and close of the files involved;
- it brings the guest's view to the daemon's: the directories involved lose
  their complete mark, so the next lookup or listing asks the daemon, the
  dentries of the names and the attributes of the inodes are revalidated on
  next use, and a node whose CREATE or MKDIR was refused becomes a bad inode,
  whose calls fail and whose dentries are dropped;
- entries that depended on a failed one fail on the daemon by themselves.

An entry whose reply never arrived (a transport failure) is treated the same
way, except that its node is not marked bad: revalidation finds it if the
daemon created it.

A program that needs to know that a name exists on the daemon calls `fsync` on
the file and on its directory, or `syncfs`. Mounting with `-o sync` (or
`-o dirsync` for names only) restores the behaviour of v0: each call is one
trap and returns the daemon's result, and with `sync` a close writes the
file's data back and waits.

### Counters

`/sys/kernel/debug/ovnfs/calls` has one line per operation, `NAME CALLS
NANOSECONDS` (the calls' total time), including `compound` and `alloc_ids`,
then lines `NAME COUNT 0` for event counters and the journal's high-water
mark:

| line | counts |
| --- | --- |
| `batched` | sub-requests sent inside COMPOUND calls |
| `journal_create`, `journal_mkdir`, `journal_unlink`, `journal_rmdir`, `journal_rename`, `journal_link`, `journal_setattr`, `journal_data` | entries journaled, by kind |
| `create_data` | CREATEs that carried their file's content |
| `id_refills` | ALLOC_IDS refills of the id pool |
| `late_failures` | entries the daemon refused or did not answer |
| `journal_high` | the most entries any journal held at once |

## Remaining work

| area | what is missing |
| --- | --- |
| Testing | the module has been exercised by the benchmark's workloads and its `verify` checks (`verify.tree`, `verify.data`, `verify.churn`) and, with temporary builds that are not in the tree, by failure exercises: each journaled operation refused by the daemon, a 64-entry journal bound and ALLOC_IDS refused. No xfstests, pjdfstest, fsx or fsstress, no fuzzing of the daemon's decoder, no test of snapshot, restore or fork with a mounted tree. The daemon's server has protocol, concurrency and workload tests (see [development](development.md#overlay-and-kernel-share-tests)) |
| Debug kernels | no run under a kernel with lockdep or KASAN |
| Ids for SYMLINK and MKNOD | the contract and the daemon give guest-chosen ids to CREATE and MKDIR only, so a symlink-heavy install still traps once per link |
| Transport failure | a transport failure in the middle of a COMPOUND with the new entry kinds (a CREATE with content, a SETATTR with the whole content, DATA runs) has not been exercised |
| Mapped files | a writable mapping racing a journaled truncation is not covered by any test |
| Changes made elsewhere | the epoch changes when the daemon changes the checkout (`pull`, reattach, removal) or restarts. A change made through another frontend of the same checkout (the host's own mount, a second VM) does not change it, so a guest keeps what it cached of the changed files |
| Ownership | new files are owned by the daemon's user, whoever created them in the guest; a non-root guest user is then refused access to its own files. remote-fs's id-range maps are to be shared with the kernel share |
| Creation cost | sustained small-file creation is bounded by the host's cost of writing each file out once the memory tier's budget is full (30-45 µs per file on APFS) |
| Renames elsewhere | the lock that orders operations by id against renames is not taken by renames through the checkout's other frontends (the host's mount, NFS, SMB), so such a rename can still fail or misdirect an operation by id in flight |
| Read path | file contents are copied three times between the daemon's file and the guest's page cache; the VMM reading the backing file of immutable content straight into guest pages is the planned fix |
| Semantics | a file unlinked while open lives only in the guest's page cache from then on; `RENAME_NOREPLACE` is a check followed by a move on the daemon; when the daemon connection breaks after it executed a request, the VMM's single resend can execute it twice |
| Platforms | the module is built and run for aarch64 on Hypervisor.framework only; x86_64 and KVM hosts are untested. The guest finds the call page at a fixed address per architecture, not through the device tree or ACPI |
| Integration | vibevmm's agent does not load the module and its node does not create kernel shares yet; the remote-fs v1 server stays until they do |

## Baseline (M0)

Measured on 2026-11-20 with `scripts/guestbench` (see
[Guest comparisons](development.md#guest-comparisons)) on one host, an Apple
M4 Max (16 cores, 128 GB) running macOS 27.2.1. The VMM is vibevmm `vmon`
0.4.1 (commit `f7c3897`) on Hypervisor.framework; the guest kernel is
6.16.9-vmon (image sha256 prefix `9d613247653f7e6b`). The ovnfs working tree
is at `3e0fd37` plus this harness. Guests have 4 vCPUs and 8 GiB. The fixture
is a synthetic crate of 100 modules plus 50,000 bulk files (50,306 entries,
50,102 files in the git index). Each phase runs 3 rounds and the tables report
medians; `cargo.cold` and the network calibration are measured once per VM.

The four columns are:

- `remotefs`: the remote-fs v1 virtio-fs device;
- `nfs3` and `nfs4`: the guest NFS client with default mount options, through
  the VMM's user-mode NAT;
- `ext4`: the fixture on the guest's root disk (virtio-blk), the ceiling.

`.cold` phases remount the tree and drop the guest's caches. `git` runs with
the git directory on the guest's disk for every column.

### Wall time (median)

| phase | remotefs | nfs3 | nfs4 | ext4 |
| --- | ---: | ---: | ---: | ---: |
| net.rr | 177.4ms | 157.9ms | 153.3ms | 259.1ms |
| net.up | 915.0ms | 1.056s | 916.8ms | 1.990s |
| net.down | 335.4ms | 315.4ms | 319.4ms | 698.5ms |
| walk.cold | 3.418s (14.48x) | 4.673s (19.80x) | 5.180s (21.95x) | 236.0ms |
| walk.warm | 3.490s (42.13x) | 5.111s (61.70x) | 5.284s (63.79x) | 82.8ms |
| git.cold | 4.361s (32.84x) | 995.5ms (7.50x) | 1.818s (13.69x) | 132.8ms |
| git.warm | 3.964s (104.83x) | 50.9ms (1.35x) | 89.0ms (2.35x) | 37.8ms |
| rg.cold | 104.714s (46.55x) | 7.959s (3.54x) | 23.226s (10.32x) | 2.250s |
| rg.warm | 153.260s (1449.52x) | 3.411s (32.26x) | 13.961s (132.04x) | 105.7ms |
| fio.seq-write | 11.594s (77.81x) | 1.078s (7.23x) | 971.0ms (6.52x) | 149.0ms |
| fio.seq-read | 468.0ms (13.76x) | 359.0ms (10.56x) | 285.0ms (8.38x) | 34.0ms |
| fio.rand-write | 2.908s (12.02x) | 478.0ms (1.98x) | 522.0ms (2.16x) | 242.0ms |
| fio.rand-read | 1.224s (0.86x) | 2.471s (1.73x) | 2.374s (1.66x) | 1.426s |
| small.create | 2.593s (60.29x) | 52.311s (1216.24x) | 52.783s (1227.22x) | 43.0ms |
| small.stat | 391.6ms (78.34x) | 303.0ms (60.60x) | 353.1ms (70.64x) | 5.0ms |
| small.rename | 22.012s (1190.81x) | 1.581s (85.55x) | 1.609s (87.04x) | 18.5ms |
| small.unlink | 14.619s (640.69x) | 740.2ms (32.44x) | 778.1ms (34.10x) | 22.8ms |
| replay.cold | 16.024s (336.68x) | 7.914s (166.28x) | 11.626s (244.27x) | 47.6ms |
| replay.no-op | 3.892s (1179.47x) | 172.1ms (52.17x) | 684.0ms (207.30x) | 3.3ms |
| replay.touch | 6.523s (340.61x) | 1.500s (78.33x) | 1.939s (101.23x) | 19.2ms |
| replay.clean | 806.9ms (90.34x) | 280.5ms (31.40x) | 561.4ms (62.85x) | 8.9ms |
| cargo.cold | 3.868s (4.05x) | 1.417s (1.49x) | 2.314s (2.42x) | 954.2ms |
| cargo.no-op | 539.2ms (6.15x) | 93.7ms (1.07x) | 101.3ms (1.16x) | 87.7ms |
| cargo.touch | 6.471s (24.64x) | 622.0ms (2.37x) | 994.6ms (3.79x) | 262.6ms |

### Units and daemon (median)

calls/op is the checkout's daemon file calls divided by the phase's guest operations; daemon ms sums those calls' durations, so it exceeds wall time when calls overlap.

| phase | remotefs unit | remotefs calls/op | remotefs daemon ms | nfs3 unit | nfs3 calls/op | nfs3 daemon ms | nfs4 unit | nfs4 calls/op | nfs4 daemon ms | ext4 unit |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| net.rr | 88.7 µs/op | 0.00 | 0.0 | 79.0 µs/op | 0.00 | 0.0 | 76.6 µs/op | 0.00 | 0.0 | 129.5 µs/op |
| net.up | 279.8 MiB/s |  | 0.0 | 242.4 MiB/s |  | 0.0 | 279.2 MiB/s |  | 0.0 | 128.6 MiB/s |
| net.down | 763.2 MiB/s |  | 0.0 | 811.8 MiB/s |  | 0.0 | 801.6 MiB/s |  | 0.0 | 366.5 MiB/s |
| walk.cold | 67.9 µs/op | 2.02 | 558.3 | 92.9 µs/op | 1.85 | 441.6 | 103.0 µs/op | 2.82 | 534.6 | 4.7 µs/op |
| walk.warm | 69.4 µs/op | 2.02 | 567.5 | 101.6 µs/op | 2.00 | 364.8 | 105.0 µs/op | 2.01 | 363.1 | 1.6 µs/op |
| git.cold | 87.0 µs/op | 2.04 | 689.3 | 19.9 µs/op | 2.06 | 390.4 | 36.3 µs/op | 3.07 | 577.1 | 2.7 µs/op |
| git.warm | 79.1 µs/op | 2.04 | 691.8 | 1.0 µs/op | 0.01 | 0.9 | 1.8 µs/op | 0.02 | 2.6 | 0.8 µs/op |
| rg.cold | 2090.0 µs/op | 13.22 | 76508.0 | 158.9 µs/op | 9.90 | 2636.5 | 463.6 µs/op | 11.11 | 3225.2 | 44.9 µs/op |
| rg.warm | 3058.9 µs/op | 13.25 | 123811.3 | 68.1 µs/op | 3.80 | 528.7 | 278.7 µs/op | 7.01 | 862.1 | 2.1 µs/op |
| fio.seq-write | 45289.1 µs/op, 22.1 MiB/s | 1024.29 | 7175.4 | 4210.9 µs/op, 237.5 MiB/s | 4.10 | 93.7 | 3793.0 µs/op, 263.6 MiB/s | 5.13 | 47.0 | 582.0 µs/op, 1718.1 MiB/s |
| fio.seq-read | 1828.1 µs/op, 547.0 MiB/s | 24.07 | 64.4 | 1402.3 µs/op, 713.1 MiB/s | 4.08 | 28.6 | 1113.3 µs/op, 898.2 MiB/s | 4.12 | 23.9 | 132.8 µs/op, 7529.4 MiB/s |
| fio.rand-write | 177.5 µs/op, 22.0 MiB/s | 4.00 | 497.4 | 29.2 µs/op, 133.9 MiB/s | 2.65 | 215.7 | 31.9 µs/op, 122.6 MiB/s | 3.32 | 14891.3 | 14.8 µs/op, 264.5 MiB/s |
| fio.rand-read | 74.7 µs/op, 52.3 MiB/s | 3.00 | 306.9 | 150.8 µs/op, 25.9 MiB/s | 4.00 | 421.2 | 144.9 µs/op, 27.0 MiB/s | 4.00 | 322.9 | 87.0 µs/op, 44.9 MiB/s |
| small.create | 259.3 µs/op | 34.16 | 792.7 | 5231.1 µs/op | 15.09 | 189015.1 | 5278.3 µs/op | 20.12 | 107166.2 | 4.3 µs/op |
| small.stat | 39.2 µs/op | 2.02 | 151.3 | 30.3 µs/op | 1.92 | 13.7 | 35.3 µs/op | 1.92 | 5.7 | 0.5 µs/op |
| small.rename | 2201.2 µs/op | 28.98 | 2532.9 | 158.1 µs/op | 14.00 | 3573.8 | 160.9 µs/op | 14.00 | 3835.5 | 1.8 µs/op |
| small.unlink | 1461.9 µs/op | 8.09 | 2219.0 | 74.0 µs/op | 5.06 | 1895.1 | 77.8 µs/op | 5.05 | 1883.4 | 2.3 µs/op |
| replay.cold |  |  | 11877.4 |  |  | 19222.0 |  |  | 18366.0 |  |
| replay.no-op |  |  | 3470.5 |  |  | 10.8 |  |  | 6.3 |  |
| replay.touch |  |  | 4516.3 |  |  | 4032.6 |  |  | 3651.1 |  |
| replay.clean |  |  | 271.9 |  |  | 92.4 |  |  | 149.0 |  |
| cargo.cold |  |  | 1236.5 |  |  | 1448.2 |  |  | 1514.1 |  |
| cargo.no-op |  |  | 309.1 |  |  | 6.1 |  |  | 6.6 |  |
| cargo.touch |  |  | 4295.6 |  |  | 184.7 |  |  | 202.5 |  |

### Reading the numbers

- Host noise is large: across the three rounds several phases spread 2x to
  10x between fastest and slowest (I/O phases most), and an ext4-only rerun
  differed from the matrix's ext4 column by up to 2.4x (`rg.cold` 931.8ms
  against 2.250s). Ratios below about 3x are within noise; the conclusions
  rest on gaps of 10x and more.
- The network calibration bounds the NFS columns: one TCP round trip through
  the user-mode NAT took 76.6 to 129.5 microseconds across the four VMs of
  the matrix and 285.6 microseconds in the ext4-only rerun.
- `daemon ms` is summed call latency, not CPU; daemon CPU over each config's
  whole run was 231 s (remotefs, 1369 s wall), 98 s (nfs3, 357 s) and 115.9 s
  (nfs4, 443 s).
- Limits of the baseline: one macOS host; NFS only through user-mode NAT (no
  KVM host with TAP was measured); NFSv4.0 delegations need a callback
  connection to the guest, which NAT prevents `[INFERENCE: not verified from
  server counters]`; NFS transfer size left at the daemon's 128 KiB default.

## Findings

1. Every uncached operation costs a round trip, and a round trip costs tens of
   microseconds on any path: `small.stat` is 39.2 (remotefs), 30.3 (nfs3) and
   35.3 (nfs4) microseconds per file against 0.5 on ext4. No per-operation
   protocol reaches the ceiling; only answering from guest caches does.
2. Time-based caches do not survive real walks: `walk.warm` equals
   `walk.cold` on NFS (5.111s against 4.673s on nfs3) because a walk outlasts
   the client's attribute timeout, and remote-fs revalidates after one
   second. `git.warm` on NFS is fast (50.9ms) only because it follows
   `git.cold` within that timeout.
3. NFS creates are bound by durability, not by the network: `small.create`
   takes 5231.1 microseconds per file on nfs3; the daemon's timers show CREATE
   at 7.6 ms and WRITE at 11.53 ms per call (four writers in parallel), with
   no COMMIT, i.e. the client's stable write on close is synced per file
   `[INFERENCE: read from the timers, not confirmed in the server code]`. The
   kernel client's server must not sync per operation; durability belongs to
   an explicit fsync.
4. remote-fs v1 is not a meaningful bar. Besides its protocol (one request in
   flight, JSON, a path walk per request), the measurements expose separate
   defects: reads cost 2 to 3 ms per file (`rg.cold` 2090.0, `rg.warm` 3058.9
   microseconds per file) because every close sends a sync that the server
   answers by opening and syncing the file, even when it was only read
   `[INFERENCE: from remotefs/ops.go sync(); on macOS os.File.Sync is
   F_FULLFSYNC]`; sequential writes arrive in 4 KiB requests (1024.29 daemon
   calls per MiB, 22.1 MiB/s); rename and unlink slow down with the number of
   inodes the VMM has seen (`small.rename` 2201.2 microseconds per file here,
   478.7 on a 2,000-file fixture).
5. git cannot run inside a checkout from a guest today: a checkout's `.git` is
   a file naming the private git directory by its host path. The kernel
   client's protocol must present `.git` as that directory (requirement for
   M1).
6. A vibevmm guest boots with its clock at the Unix epoch (no RTC). Mtimes
   written by the guest and by the daemon then disagree by decades, which
   breaks cargo's and make's freshness checks on any network filesystem;
   whoever boots the guest must set its clock (the harness does, from the
   host).
7. Where the compiler dominates, the filesystem matters little: `cargo.cold`
   is 1.49x of ext4 on nfs3. The replayed build, which is cargo's file
   traffic without rustc, is where the paths differ: `replay.cold` 166.28x
   (nfs3), 336.68x (remotefs).

## Targets and decision

Decision: **go**. The NFS path that exists today is 20x off the ceiling on a
cold walk, 62x on a warm one, 1216x on small-file creation and 166x on the
replayed build, and its per-operation round trips and time-based caches are
structural; only the server's sync policy is tunable. Stop criterion: if at
the end of M5 the warm metadata phases are not within 2x of ext4 at zero
daemon calls, the project stops.

Targets are measured on this harness and host, as ratios against the same
run's ext4 column. The `ext4` and `best existing path` cells quote the tables
above: the fastest of remotefs, nfs3 and nfs4 for that phase, named in
parentheses; per-file and throughput phases give the unit the target uses.

| phase | ext4 | best existing path | target | milestone | v0 |
| --- | --- | --- | --- | --- | --- |
| `walk.cold` | 236.0ms | 3.418s, 14.48x (remotefs) | at most 3x ext4 and at most 0.05 daemon calls per entry (one directory read per directory) | M5 | 1.37x and one directory read per directory: met |
| `walk.warm`<br>`git.warm`<br>`rg.warm`<br>`small.stat`<br>`replay.no-op`<br>`cargo.no-op` | 82.8ms<br>37.8ms<br>105.7ms<br>5.0ms<br>3.3ms<br>87.7ms | 3.490s, 42.13x (remotefs)<br>50.9ms, 1.35x (nfs3)<br>3.411s, 32.26x (nfs3)<br>303.0ms, 60.60x (nfs3)<br>172.1ms, 52.17x (nfs3)<br>93.7ms, 1.07x (nfs3) | at most 2x ext4 and zero daemon calls | M5 | 0.69x<br>0.81x<br>0.94x<br>0.79x<br>0.86x<br>0.97x<br>zero round trips: met |
| `git.cold` | 132.8ms | 995.5ms, 7.50x (nfs3) | at most 3x ext4 | M5 | 1.54x: met |
| `rg.cold` | 2.250s | 7.959s, 3.54x (nfs3) | at most 2x ext4 and at most 1.1 daemon calls per file | M6 | 1.55x and one round trip per file: met |
| `small.create` | 4.3 µs/op | 259.3 µs/op, 2.593s, 60.29x (remotefs) | at most 100 microseconds per file and at most 2 daemon calls per file | M4 | 30.8 µs per file and two round trips: met |
| `small.rename`<br>`small.unlink` | 1.8 µs/op<br>2.3 µs/op | 158.1 µs/op, 1.581s, 85.55x (nfs3)<br>74.0 µs/op, 740.2ms, 32.44x (nfs3) | at most 60 microseconds per file and 1 daemon call per file | M4 | 19.1 µs per file<br>18.5 µs per file<br>one round trip each: met |
| `replay.cold`<br>`replay.touch` | 47.6ms<br>19.2ms | 7.914s, 166.28x (nfs3)<br>1.500s, 78.33x (nfs3) | at most 6x ext4 | M6 | 6.62x: not met<br>1.87x: met |
| `cargo.cold` | 954.2ms | 1.417s, 1.49x (nfs3) | at most 1.15x ext4 | M6 | 1.06x: met |
| `cargo.touch` | 262.6ms | 622.0ms, 2.37x (nfs3) | at most 1.3x ext4 | M6 | 1.45x: not met |
| `fio.seq-write` | 1718.1 MiB/s | 263.6 MiB/s, 971.0ms, 6.52x (nfs4) | at least 1 GiB/s | M6 | 1488.4 MiB/s: met |
| `fio.seq-read` | 7529.4 MiB/s | 898.2 MiB/s, 285.0ms, 8.38x (nfs4) | at least 3 GiB/s | M6 | 4571.4 MiB/s: met |
| `fio.rand-read`<br>`fio.rand-write` | 1.426s<br>242.0ms | 1.224s, 0.86x (remotefs)<br>478.0ms, 1.98x (nfs3) | at most 1.5x ext4 | M6 | 0.51x: met<br>1.63x: not met |

The targets' "daemon calls" are round trips as the module counts them; the
harness's `calls/op` column counts the daemon's internal file calls, several
per round trip.

The per-file bounds on creates, renames and unlinks assume one synchronous
round trip of 30 to 40 microseconds per mutation on this host
`[INFERENCE: estimated from small.stat on remote-fs, 39.2 microseconds at 2.02
daemon calls]`; asynchronous directory operations could lower them later.

## First version (v0)

Measured on 2026-11-20 with `scripts/guestbench` on the same host, kernel,
vmon version and fixture as the baseline, in one session of 3 rounds with
medians reported. The columns are `nfs3`, `ext4`, `erofs` (the image
prototype, described below) and `kmod` (the kernel client: module `ovnfs.ko`,
daemon share `--kernel`, vmon `--ovnfs`). The host was shared and loaded
throughout (load average 21.87 when the run began, 7.56 when it ended), and
`nfs3` ran first, under the heaviest load: several of its rows are worse than
in the baseline, so comparisons with NFS below use NFS's best value over all
sessions of the day. The `net.*` rows are the network calibration and say
nothing about `ext4`, `erofs` or `kmod`.

### Wall time (median)

| phase | nfs3 | ext4 | erofs | kmod |
| --- | ---: | ---: | ---: | ---: |
| net.rr | 303.6ms | 141.9ms | 143.8ms | 158.9ms |
| net.up | 5.097s | 970.4ms | 924.8ms | 993.9ms |
| net.down | 2.548s | 289.9ms | 310.1ms | 356.6ms |
| walk.cold | 4.591s (39.91x) | 115.0ms | 164.2ms (1.43x) | 157.1ms (1.37x) |
| walk.warm | 5.117s (106.23x) | 48.2ms | 39.1ms (0.81x) | 33.4ms (0.69x) |
| git.cold | 858.6ms (18.01x) | 47.7ms | 63.3ms (1.33x) | 73.4ms (1.54x) |
| git.warm | 45.5ms (1.87x) | 24.4ms | 14.5ms (0.60x) | 19.7ms (0.81x) |
| rg.cold | 6.853s (10.74x) | 638.0ms | 193.6ms (0.30x) | 987.3ms (1.55x) |
| rg.warm | 3.085s (40.22x) | 76.7ms | 89.0ms (1.16x) | 71.8ms (0.94x) |
| fio.seq-write | 1.160s (9.67x) | 120.0ms | 96.0ms (0.80x) | 172.0ms (1.43x) |
| fio.seq-read | 283.0ms (11.32x) | 25.0ms | 26.0ms (1.04x) | 56.0ms (2.24x) |
| fio.rand-write | 399.0ms (1.91x) | 209.0ms | 196.0ms (0.94x) | 340.0ms (1.63x) |
| fio.rand-read | 2.432s (4.12x) | 591.0ms | 459.0ms (0.78x) | 299.0ms (0.51x) |
| small.create | 68.377s (2514.27x) | 27.2ms | 29.0ms (1.07x) | 308.4ms (11.34x) |
| small.stat | 923.1ms (394.38x) | 2.3ms | 2.5ms (1.05x) | 1.8ms (0.79x) |
| small.rename | 5.486s (361.67x) | 15.2ms | 18.4ms (1.21x) | 191.1ms (12.60x) |
| small.unlink | 1.855s (103.76x) | 17.9ms | 16.0ms (0.89x) | 185.2ms (10.36x) |
| replay.cold | 17.292s (580.86x) | 29.8ms | 83.0ms (2.79x) | 197.0ms (6.62x) |
| replay.no-op | 169.2ms (52.92x) | 3.2ms | 4.3ms (1.34x) | 2.8ms (0.86x) |
| replay.touch | 2.525s (149.35x) | 16.9ms | 58.0ms (3.43x) | 31.6ms (1.87x) |
| replay.clean | 1.586s (201.35x) | 7.9ms | 10.0ms (1.27x) | 73.3ms (9.31x) |
| cargo.cold | 3.992s (4.63x) | 861.4ms | 1.146s (1.33x) | 913.1ms (1.06x) |
| cargo.no-op | 102.5ms (1.28x) | 80.1ms | 78.3ms (0.98x) | 77.8ms (0.97x) |
| cargo.touch | 1.666s (7.78x) | 214.0ms | 201.8ms (0.94x) | 310.0ms (1.45x) |
| verify.tree | 12.732s (15.07x) | 845.1ms | 289.0ms (0.34x) | 1.373s (1.62x) |
| verify.data |  | 16.4ms | 13.7ms (0.84x) | 17.8ms (1.08x) |

### Units and daemon (median)

calls/op is the checkout's daemon file calls divided by the phase's guest operations; daemon ms sums those calls' durations, so it exceeds wall time when calls overlap.

| phase | nfs3 unit | nfs3 calls/op | nfs3 daemon ms | ext4 unit | erofs unit | kmod unit | kmod calls/op | kmod daemon ms |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| net.rr | 151.8 µs/op | 0.00 | 0.0 | 70.9 µs/op | 71.9 µs/op | 79.5 µs/op | 0.00 | 0.0 |
| net.up | 50.2 MiB/s |  | 0.0 | 263.8 MiB/s | 276.8 MiB/s | 257.6 MiB/s |  | 0.0 |
| net.down | 100.5 MiB/s |  | 0.0 | 883.2 MiB/s | 825.4 MiB/s | 717.9 MiB/s |  | 0.0 |
| walk.cold | 91.3 µs/op | 1.85 | 281.8 | 2.3 µs/op | 3.3 µs/op | 3.1 µs/op | 0.01 | 90.9 |
| walk.warm | 101.7 µs/op | 2.00 | 347.5 | 1.0 µs/op | 0.8 µs/op | 0.7 µs/op | 0.00 | 0.0 |
| git.cold | 17.1 µs/op | 2.06 | 264.9 | 1.0 µs/op | 1.3 µs/op | 1.5 µs/op | 0.01 | 96.0 |
| git.warm | 0.9 µs/op | 0.01 | 0.9 | 0.5 µs/op | 0.3 µs/op | 0.4 µs/op | 0.00 | 0.0 |
| rg.cold | 136.8 µs/op | 9.90 | 2283.9 | 12.7 µs/op | 3.9 µs/op | 19.7 µs/op | 5.01 | 1955.2 |
| rg.warm | 61.6 µs/op | 3.80 | 489.5 | 1.5 µs/op | 1.8 µs/op | 1.4 µs/op | 0.00 | 0.0 |
| fio.seq-write | 4531.2 µs/op, 220.7 MiB/s | 4.10 | 146.8 | 468.8 µs/op, 2133.3 MiB/s | 375.0 µs/op, 2666.7 MiB/s | 671.9 µs/op, 1488.4 MiB/s | 3.02 | 84.6 |
| fio.seq-read | 1105.5 µs/op, 904.6 MiB/s | 4.08 | 26.5 | 97.7 µs/op, 10240.0 MiB/s | 101.6 µs/op, 9846.2 MiB/s | 218.8 µs/op, 4571.4 MiB/s | 2.02 | 15.3 |
| fio.rand-write | 24.4 µs/op, 160.4 MiB/s | 2.65 | 202.5 | 12.8 µs/op, 306.2 MiB/s | 12.0 µs/op, 326.5 MiB/s | 20.8 µs/op, 188.2 MiB/s | 1.99 | 156.0 |
| fio.rand-read | 148.4 µs/op, 26.3 MiB/s | 4.00 | 421.6 | 36.1 µs/op, 108.3 MiB/s | 28.0 µs/op, 139.4 MiB/s | 18.2 µs/op, 214.0 MiB/s | 2.00 | 27.8 |
| small.create | 6837.7 µs/op | 15.09 | 247577.9 | 2.7 µs/op | 2.9 µs/op | 30.8 µs/op | 6.04 | 486.9 |
| small.stat | 92.3 µs/op | 1.97 | 20.5 | 0.2 µs/op | 0.2 µs/op | 0.2 µs/op | 0.00 | 0.0 |
| small.rename | 548.6 µs/op | 14.00 | 10270.5 | 1.5 µs/op | 1.8 µs/op | 19.1 µs/op | 4.00 | 493.9 |
| small.unlink | 185.5 µs/op | 5.06 | 2793.2 | 1.8 µs/op | 1.6 µs/op | 18.5 µs/op | 3.03 | 480.6 |
| replay.cold |  |  | 45542.2 |  |  |  |  | 280.9 |
| replay.no-op |  |  | 5.9 |  |  |  |  | 0.0 |
| replay.touch |  |  | 7838.5 |  |  |  |  | 13.2 |
| replay.clean |  |  | 1318.6 |  |  |  |  | 41.4 |
| cargo.cold |  |  | 5404.2 |  |  |  |  | 70.4 |
| cargo.no-op |  |  | 14.4 |  |  |  |  | 0.0 |
| cargo.touch |  |  | 1185.9 |  |  |  |  | 84.8 |
| verify.tree | 254.1 µs/op, 0.1 MiB/s | 8.03 | 31070.6 | 16.9 µs/op, 1.2 MiB/s | 5.8 µs/op, 3.6 MiB/s | 27.4 µs/op, 0.7 MiB/s | 5.01 | 1806.0 |
| verify.data |  |  |  | 964.6 µs/op, 952.2 MiB/s | 807.2 µs/op, 1137.9 MiB/s | 1044.6 µs/op, 879.2 MiB/s | 3.12 | 1.4 |

For `kmod`, `calls/op` counts the daemon's file calls, of which one request
makes several; the module's own counters below count round trips.

### Round trips

The module's counters over the whole `kmod` run (all workloads and rounds);
mean is the guest-observed time per trap.

| call | count | mean |
| --- | ---: | ---: |
| hello | 22 | 63.7 µs |
| poll | 49 | 257.3 µs |
| statfs | 20 | 49.3 µs |
| getattr | 3 | 16.4 µs |
| setattr | 831 | 49.0 µs |
| lookup | 8 | 28.3 µs |
| readdir | 4,473 | 321.9 µs |
| read | 301,195 | 53.3 µs |
| write | 68,838 | 44.7 µs |
| fsync | 7 | 50,913.1 µs |
| create | 34,926 | 60.4 µs |
| mkdir | 1,510 | 135.0 µs |
| link | 853 | 333.3 µs |
| unlink | 35,775 | 64.6 µs |
| rmdir | 1,518 | 98.9 µs |
| rename | 30,502 | 69.0 µs |

A walk, a `git status` and every stat of the 50,000-file tree together cost 3
GETATTR and 8 LOOKUP round trips; the tree's names and attributes arrive
through 4,473 READDIR calls.

### Against NFSv3

`NFSv3 best of the day` is the best value over all sessions of the day; the
factor is that value divided by `kmod` (the four `fio` rows are throughput, so
their factor is `kmod` divided by NFS).

| phase | kmod | NFSv3 best of the day | factor |
| --- | ---: | ---: | ---: |
| walk.cold | 157.1ms | 4.240s | 27x |
| walk.warm | 33.4ms | 4.323s | 129x |
| git.cold | 73.4ms | 858.6ms | 11.7x |
| git.warm | 19.7ms | 45.5ms | 2.3x |
| rg.cold | 987.3ms | 6.853s | 6.9x |
| rg.warm | 71.8ms | 3.085s | 43x |
| small.create | 30.8 µs/file | 5175.4 µs/file | 168x |
| small.stat | 0.2 µs/file | 29.0 µs/file | 145x |
| small.rename | 19.1 µs/file | 149.2 µs/file | 7.8x |
| small.unlink | 18.5 µs/file | 73.2 µs/file | 4.0x |
| replay.cold | 197.0ms | 7.914s | 40x |
| replay.touch | 31.6ms | 1.500s | 47x |
| replay.clean | 73.3ms | 280.5ms | 3.8x |
| cargo.cold | 913.1ms | 1.417s | 1.6x |
| cargo.touch | 310.0ms | 622.0ms | 2.0x |
| fio.seq-write | 1488.4 MiB/s | 269.8 MiB/s (nfs4) | 5.5x |
| fio.seq-read | 4571.4 MiB/s | 1113.0 MiB/s | 4.1x |
| fio.rand-write | 188.2 MiB/s | 160.4 MiB/s | 1.2x |
| fio.rand-read | 214.0 MiB/s | 32.7 MiB/s | 6.5x |

### Correctness

The `verify` workload has three phases. `verify.tree` hashes every tracked
file against git's blob ids, and `verify.data` runs sixteen written-data cases:
page and megabyte boundaries, back-to-front and mid-file rewrites, truncation
up and down, a shared writable mapping, rename and hard links, each read back
after the guest's caches are dropped. `verify.churn` runs four workers through
seeded random operation sequences on a few colliding names and checks every
operation's outcome and the whole subtree against a POSIX model, after the
operations, after a cold reset and again after `syncfs` and a wait (see
[development](development.md#guest-comparisons)); it is the check on the
journal's ordering, since the guest answers most of those operations before
the daemon has seen them. In the v0 run `verify.tree` and `verify.data` passed
on `kmod`, `erofs`, `ext4` and NFSv4. They fail on NFSv3: a 2 MiB file
truncated to 4,097 bytes and then appended with 100 bytes reads back with
2,097,152 bytes after a remount (an existing defect of the NFSv3 path with the
Linux client, not investigated; repro `-configs nfs3 -workloads verify`), and
on remote-fs v1, which has no hard links. The guest kernel's log after a full
`kmod` run held no warning, BUG or oops.

The journal's failure paths are not reachable from a healthy daemon, so they
were exercised with temporary builds: each journaled operation refused by the
daemon after its syscall returned, a journal bound of 64 entries, and ALLOC_IDS
refused. These are not tests in the tree, and the rest of the testing is
listed under [Remaining work](#remaining-work).

### What the numbers say

1. Everything answerable from the guest's caches is at or below the ext4
   column: the warm phases, and the cold walk and `git status` at one
   directory read per directory.
2. In v0 mutations were 10 to 13 times ext4: each was one synchronous round
   trip of 60 to 70 microseconds, nearly all of it in the daemon, where every
   mutation took one checkout-wide overlay lock across a host filesystem call
   (measured on APFS without contention: about 67 microseconds to unlink, 87
   to rename, 280 to hard-link, 35 to open), so four guest CPUs mutated no
   faster than one. Both halves have been changed since: the overlay applies
   the host side of a name change behind the call when the memory tier is on
   (see [internals](internals.md#memory-tiers)), and the module no longer
   makes the guest wait for the daemon at all for the operations it can
   predict ([The journal](#the-journal)), so a mutation costs the guest its
   caches' update and a share of a batch.
3. Bulk I/O was limited by macOS's 8 KiB default buffers on Unix stream
   sockets until both ends raised them to 2 MiB: sequential read went from
   318.4 to 4571.4 MiB/s and sequential write from 280.1 to 1488.4 MiB/s
   between two runs of the same day.
4. `rg.cold` and `verify.tree` (one READ round trip per small file, 53
   microseconds each) are where a per-file round trip still shows: 1.55x and
   1.62x of ext4.

### Image prototype (erofs)

The `erofs` column is an EROFS image of the tree, built by `mkfs.erofs` (6 MiB
for this fixture, 3.7 to 6.0 s through docker), appended to the guest's root
disk and mounted as the read-only lower of an overlayfs whose upper is the
guest's disk. Nothing but block reads leaves the guest, and no new kernel code
is involved.

It is within noise of ext4 on every row because it has no round trips at all,
mutations included. The cost is that the guest holds a snapshot: the host sees
none of its writes until the guest hands its upper back, and a change on the
host needs a remount. It is kept in the harness as the yardstick for what zero
round trips buy; the kernel client keeps the tree live in both directions that
the daemon controls.

## With the journal (v1)

Measured on 2026-11-20 and 2026-11-21 with `scripts/guestbench`, same host,
kernel and guest image as v0. The host was shared and its load average moved
between 4 and 60 during the work, so every comparison below is interleaved:
the committed v0 build (daemon and module of the first version) and the
current build alternate, four runs each, with `ext4` measured in the same VM
session as each current run. Fixture of 2,000 files, 3 rounds per run with the
median taken, 3 cold builds per run; each cell is the range of the four runs'
medians. `small.*` phases cover 10,000 files of 4 KiB on 4 workers.

### Wall time (ms per phase)

| phase | v0 | v1 | ext4 | v1 / ext4 |
| --- | ---: | ---: | ---: | ---: |
| small.create | 289 to 1036 | 24.2 to 37.8 | 22.2 to 28.5 | about 1.1x |
| small.rename | 193 to 628 | 11.7 to 18.8 | 12.9 to 14.9 | about 0.9x |
| small.unlink | 189 to 2604 | 4.6 to 5.3 | 22.4 to 24.2 | about 0.2x |
| replay.cold | 180 to 738 | 38.6 to 56.6 | 28.7 to 63.1 | about 1.2x |
| replay.touch | 28.6 to 43.5 | 13.9 to 16.8 | 14.9 to 30.2 | about 0.9x |
| replay.clean | 66 to 182 | 1.9 to 3.2 | 7.6 to 9.4 | about 0.3x |
| cargo.cold | 545 to 878 | 449 to 513 | 438 to 556 | about 1.0x |
| cargo.touch | 307 to 550 | 212 to 226 | 202 to 227 | about 1.0x |
| verify.churn | 430 to 530 | 164 to 731 | 98 to 192 | about 1.7x |

`small.sync`, the `syncfs` after the create phase, took 1.6 to 2.9 ms on v1
and 79 to 120 ms on `ext4`. The two are not the same promise: after it `ext4`
has written to its virtual disk, while the module has handed everything to the
daemon, whose memory tier still holds it. The phases where v1 is below `ext4`
are the ones whose host work happens after the syscall returns.

A full-size run (50,000 files, every workload, 3 rounds) passed on `ext4` and
`kmod` with a clean guest kernel log and no late failure, but the host's load
rose to 36 while the `kmod` column ran (its `net.*` rows, which do not touch
the filesystem, were 1.8 to 7.3 times `ext4`'s), so its timings are not used
here; the results are in `v1-journal.json`.

### Traps

In one run of `small`, `replay`, `cargo` and `verify` the v0 module made
35,743 CREATE, 37,847 WRITE, 3,211 SETATTR and 2,247 RMDIR calls. The v1 module
made about 2,900 COMPOUND calls and, of the synchronous kinds, 1 SETATTR,
1 WRITE, 3 RMDIR and 140 SYMLINK (all of `verify.churn`'s symlinks); the cold
READ and READDIR calls remain (about 4,900 and 104). The journal's high-water
mark was 9,770 to 15,576 entries of its 131,072.

One uncached round trip (`trap.one`, a `statfs` loop) had a median of 17
microseconds before the daemon and the VMM polled for the next message, and 12
in the one quiet pair measured after; with four callers the median per call
fell from 32 to 52 microseconds to 24 to 27 in all four pairs.

### What changed the numbers

1. The overlay stopped making host calls under the checkout lock and then
   stopped making them in the call at all (directory times in the index, queued
   host changes). Measured alone against v0 in the guest: create, rename and
   unlink about 2 to 10 times faster under load, and a floor of one round trip
   per mutation.
2. Batching write-back through COMPOUND removed one of a small file's two
   traps.
3. The journal removed the rest: a create, rename, unlink, link, mkdir, rmdir
   or attribute change costs the guest its own cache update, and the daemon
   executes a content-carrying create in about 3.5 microseconds while the
   memory tier has room.

The other frontends gained nothing measurable from the overlay changes: NFSv3
in the same guest took 54 to 63 s for `small.create` with the v0 daemon and 60
to 80 s with the current one, within the noise of that path.

### Correctness

`verify.tree`, `verify.data` and `verify.churn` passed on `kmod` in every run
used above. `verify.churn` passes on `ext4`, which is what validates its
model, and with the v0 module against both daemons. It found, in order: a
daemon race between operations by id and renames on another connection
(present in v0, now closed by the names lock), ordering races between
write-back, replies and truncation in the module, and three deadlocks (a read
holding page locks while waiting for the journal against a truncation, inode
eviction against `sync`, and `sync` starved by a file truncated in a loop).
After the last fix the command that had hung passed 60 consecutive runs under
load and the `verify` loop 40.

## Kernel audit

[`kernel-client-audit.md`](kernel-client-audit.md) records the pinned guest
kernel's APIs, exports, config and reference implementations checked for the
module. Three of its consequences shape the design: the module must be
GPL-licensed because every virtio core export is GPL-only; virtio-mmio gives a
device one shared interrupt; and the module must be built with the kernel's
exact config, source and compiler because nothing but vermagic and the size of
`struct module` is checked at load.
