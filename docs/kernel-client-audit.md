# Kernel client: guest kernel audit

This audit checks the assumptions the planned out-of-tree module `ovnfs.ko`
makes about the vibevmm guest kernel: a C module that talks to the host over a
virtio transport, keeps file data in the page cache through the netfs library,
is loaded with `finit_module`, and runs with module versions off and no module
signature. The design is in [kernel-client.md](kernel-client.md).

The audited tree is cloud-hypervisor/linux tag `ch-release-v6.16.9-20260508`
(tarball sha256
`a2aaf2b459842810f3521cdfe8f5fa97bd816768019e184b6dafa0c7254d6f21`), as pinned
by vibevmm's `kernel/build.sh`. It was checked against the resolved config
`target/kernel/config-aarch64` of vibevmm (sha256
`ebfab614d05b153022635148f82b28540cf241ce2b8f127d58ecbe33d4dab1e8`, cited
below as `cfg:LINE`), kernel release `6.16.9-vmon`, on 2026-11-20. The release
comes from `Makefile:2-5` plus `CONFIG_LOCALVERSION="-vmon"` in `common.config`
(cfg:33). Every `path:line` reference is relative to that kernel tree and must
be re-checked when the pin moves.

Assumed module: out-of-tree C `ovnfs.ko`, virtio transport, netfs page cache,
`finit_module`, `MODVERSIONS` off, unsigned.

## 1. netfs library

| Assumption | Verdict | Evidence |
|---|---|---|
| `struct netfs_request_ops` members | holds (shape below) | include/linux/netfs.h:288-314: `mempool_t *request_pool, *subrequest_pool; int (*init_request)(rreq, struct file *); void (*free_request)(rreq); void (*free_subrequest)(subreq); void (*expand_readahead)(rreq); int (*prepare_read)(subreq); void (*issue_read)(subreq); bool (*is_still_valid)(rreq); int (*check_write_begin)(file, pos, len, struct folio **, void **); void (*done)(rreq); void (*update_i_size)(inode, loff_t); void (*post_modify)(inode); void (*begin_writeback)(wreq); void (*prepare_write)(subreq); void (*issue_write)(subreq); void (*retry_request)(wreq, struct netfs_io_stream *); void (*invalidate_cache)(wreq);` |
| `issue_read` returns a status | differs | netfs.h:298 `void (*issue_read)(struct netfs_io_subrequest *subreq)`; result reported only via termination call |
| Read completion: `netfs_read_subreq_terminated(subreq, err, was_async)` | differs: single argument | netfs.h:434 `void netfs_read_subreq_terminated(struct netfs_io_subrequest *subreq);` EXPORT_SYMBOL fs/netfs/read_collect.c:566. Caller sets `subreq->transferred += n`, `subreq->error`, and flags `NETFS_SREQ_MADE_PROGRESS`/`HIT_EOF`/`CLEAR_TAIL` first (fs/9p/vfs_addr.c:69-92). Partial progress: `netfs_read_subreq_progress(subreq)` netfs.h:433, EXPORT_SYMBOL read_collect.c:496 |
| Write completion `netfs_write_subrequest_terminated(op, len, was_async)` | differs: two arguments | netfs.h:445 / fs/netfs/write_collect.c:475 `void netfs_write_subrequest_terminated(void *_op, ssize_t transferred_or_error);` EXPORT_SYMBOL write_collect.c:531. Short write sets NEED_RETRY (write_collect.c:525-526). Prepare failure: `netfs_prepare_write_failed(subreq)` EXPORT_SYMBOL write_issue.c:152 |
| Termination callable from IRQ/any context | holds [INFERENCE] | both only update counters, clear in-progress and put the subreq (read_collect.c:563-565, write_collect.c:528-530); collection is woken/queued, not run inline |
| `netfs_inode` init | holds | netfs.h:58 `struct netfs_inode { struct inode inode; const struct netfs_request_ops *ops; [cache if FSCACHE]; struct mutex wb_lock; loff_t remote_i_size; loff_t zero_point; atomic_t io_count; unsigned long flags; }` must embed and *start with* the VFS inode; netfs.h:488 `static inline void netfs_inode_init(struct netfs_inode *ctx, const struct netfs_request_ops *ops, bool use_zero_point)`; `netfs_resize_file(ctx, new_i_size, changed_on_server)` netfs.h:~512 |
| `netfs_file_read_iter` | holds, EXPORT_SYMBOL | netfs.h:395 `ssize_t netfs_file_read_iter(struct kiocb *, struct iov_iter *)`; buffered_read.c:834 |
| `netfs_file_write_iter` | holds, EXPORT_SYMBOL | netfs.h:405; buffered_write.c:497 |
| `netfs_read_folio` / `netfs_readahead` | holds, EXPORT_SYMBOL | netfs.h:417 `int netfs_read_folio(struct file *, struct folio *)` buffered_read.c:540; netfs.h:416 `void netfs_readahead(struct readahead_control *)` buffered_read.c:374 |
| `netfs_writepages` | holds, EXPORT_SYMBOL | netfs.h:421 `int netfs_writepages(struct address_space *, struct writeback_control *)`; write_issue.c:616 |
| `netfs_page_mkwrite` | holds, EXPORT_SYMBOL | netfs.h:430 `vm_fault_t netfs_page_mkwrite(struct vm_fault *vmf, struct netfs_group *netfs_group)`; buffered_write.c:569 |
| `netfs_dirty_folio`, `netfs_release_folio`, `netfs_invalidate_folio` | holds, EXPORT_SYMBOL | netfs.h:423,427,426; misc.c:163, 315, 283 |
| `netfs_write_begin` needed in aops | differs: not needed | 9p aops have no `write_begin`/`write_end` (fs/9p/vfs_addr.c:160-169); writes go through `netfs_perform_write` (buffered_write.c:411). `netfs_write_begin` still exported (buffered_read.c:711) |
| Unbuffered/direct I/O | holds, EXPORT_SYMBOL | `netfs_unbuffered_read_iter` direct_read.c:267, `netfs_unbuffered_write_iter` direct_write.c:182 |
| `netfs_unpin_writeback` for `->write_inode` | holds, EXPORT_SYMBOL | netfs.h:424; misc.c:181 |
| I/O locking helpers | holds, EXPORT_SYMBOL | `netfs_start_io_read/write/direct`, `netfs_end_io_*` locking.c:78-205 |
| Iterator kind given to `issue_read`/`issue_write` | holds: `subreq->io_iter`, ITER_FOLIOQ for buffered | netfs.h:178 `struct iov_iter io_iter`; buffered read: `subreq->io_iter = rreq->buffer.iter` (buffered_read.c:123) where the rolling buffer is `iov_iter_folio_queue` (rolling_buffer.c:73); writeback: `subreq->io_iter = *wreq_iter` (write_issue.c:177) over the same folio-queue buffer (write_issue.c:111). DIO: the user iter after `netfs_extract_user_iter` → ITER_BVEC (iterator.c:100); single-folio read path uses ITER_BVEC (buffered_read.c:457) |
| Module can build an sg list from io_iter | holds [INFERENCE] | use `extract_iter_to_sg()` / walk folio_queue; transport must handle ITER_FOLIOQ and ITER_BVEC (and ITER_XARRAY only on cache paths, fscache_io.c:251, unused with FSCACHE=n) |
| `NETFS_SUPPORT` is a user-selectable option | differs: promptless tristate | fs/netfs/Kconfig:3-4 `config NETFS_SUPPORT / tristate` (no prompt). Selected by 9P_FS (fs/9p/Kconfig:5), AFS (fs/afs/Kconfig:7), CEPH (fs/ceph/Kconfig:9), EROFS_FS_ONDEMAND (fs/erofs/Kconfig:12), NFS_FSCACHE (fs/nfs/Kconfig:175), CIFS (fs/smb/client/Kconfig:22) |
| `NETFS_SUPPORT=y` here | holds, by accident | cfg:3291 `CONFIG_NETFS_SUPPORT=y`; only enabled selector is `CONFIG_CIFS=y` (cfg:3431, common.config "CONFIG_CIFS=y"); NET_9P, CEPH_FS, AFS_FS, NFS_FSCACHE all off (cfg:1729, 3430, 3446, 3411). `FSCACHE` off (cfg:3294) |

Consequences: termination calls are the 6.15+ one/two-argument forms; no
`was_async`. `NETFS_SUPPORT` cannot be forced by a fragment on its own (no
prompt): it lives only as long as `CIFS=y`, so pin `CONFIG_NETFS_SUPPORT=y` in
`common.config` as a `build.sh` tripwire (`verify_config` fails if CIFS is
ever dropped). `FSCACHE=n` means `netfs_inode` has no `cache` member.

`fs/9p/vfs_addr.c` shape (smallest complete user):

```
v9fs_begin_writeback(wreq)   find writable fid, set wreq->wsize, io_streams[0].avail = true  (:33)
v9fs_issue_write(subreq)     p9_client_write(fid, subreq->start, &subreq->io_iter)          (:54)
                             set MADE_PROGRESS; netfs_write_subrequest_terminated(subreq, len ?: err)
v9fs_issue_read(subreq)      p9_client_read(fid, start+transferred, &subreq->io_iter)        (:69)
                             set CLEAR_TAIL / HIT_EOF; transferred += n; error = err;
                             netfs_read_subreq_terminated(subreq)
v9fs_init_request(rreq,file) pin fid from file or inode into rreq->netfs_priv, set wsize    (:99)
v9fs_free_request(rreq)      drop fid                                                       (:145)
v9fs_req_ops                 {init_request, free_request, issue_read, begin_writeback, issue_write} (:152)
v9fs_addr_operations         {read_folio=netfs_read_folio, readahead=netfs_readahead,
                              dirty_folio=netfs_dirty_folio, release_folio, invalidate_folio,
                              direct_IO=noop_direct_IO, writepages=netfs_writepages,
                              migrate_folio=filemap_migrate_folio}                          (:160)
fs/9p/vfs_inode.c:255         netfs_inode_init(&v9inode->netfs, &v9fs_req_ops, true)
fs/9p/vfs_file.c:364/407/480  read_iter→netfs_file_read_iter, write_iter→netfs_file_write_iter,
                              vm page_mkwrite→netfs_page_mkwrite(vmf, NULL)
```

## 2. VFS prototypes

| Assumption | Verdict | Evidence |
|---|---|---|
| `lookup` | holds | include/linux/fs.h:2223 `struct dentry *(*lookup)(struct inode *, struct dentry *, unsigned int)` |
| `create` | holds | fs.h:2230 `int (*create)(struct mnt_idmap *, struct inode *, struct dentry *, umode_t, bool)` |
| `mkdir` returns int | differs | fs.h:2236 `struct dentry *(*mkdir)(struct mnt_idmap *, struct inode *, struct dentry *, umode_t)`: return NULL (dentry used), a different dentry (e.g. from d_splice_alias), or ERR_PTR |
| `atomic_open` | holds | fs.h:2250 `int (*atomic_open)(struct inode *, struct dentry *, struct file *, unsigned open_flag, umode_t create_mode)`; `finish_open` EXPORT_SYMBOL fs/open.c:1055, `finish_no_open` open.c:1076 |
| `tmpfile` takes a `struct file` | holds | fs.h:2253 `int (*tmpfile)(struct mnt_idmap *, struct inode *, struct file *, umode_t)` |
| `rename` | holds | fs.h:2241 `int (*rename)(struct mnt_idmap *, struct inode *, struct dentry *, struct inode *, struct dentry *, unsigned int)` |
| `getattr` | holds | fs.h:2244 `int (*getattr)(struct mnt_idmap *, const struct path *, struct kstat *, u32, unsigned int)` |
| `setattr` | holds | fs.h:2243 `int (*setattr)(struct mnt_idmap *, struct dentry *, struct iattr *)` |
| `permission` | holds | fs.h:2225 `int (*permission)(struct mnt_idmap *, struct inode *, int)` |
| `get_link` | holds | fs.h:2224 `const char *(*get_link)(struct dentry *, struct inode *, struct delayed_call *)` (dentry NULL in RCU walk, Documentation/filesystems/vfs.rst:615-630) |
| `link`/`symlink`/`mknod`/`unlink`/`rmdir` | holds | fs.h:2232 `int (*link)(struct dentry *, struct inode *, struct dentry *)`; :2234 `int (*symlink)(struct mnt_idmap *, struct inode *, struct dentry *, const char *)`; :2239 `int (*mknod)(struct mnt_idmap *, struct inode *, struct dentry *, umode_t, dev_t)`; :2233 `int (*unlink)(struct inode *, struct dentry *)`; :2238 `int (*rmdir)(struct inode *, struct dentry *)` |
| `d_revalidate(struct dentry *, unsigned int)` | differs | include/linux/dcache.h:149 `int (*d_revalidate)(struct inode *dir, const struct qstr *name, struct dentry *, unsigned int)`: parent inode and a stable name are passed in, so RCU-mode revalidate need not chase `d_parent` |
| `d_weak_revalidate`, `d_prune` | holds | dcache.h:151 `int (*d_weak_revalidate)(struct dentry *, unsigned int)`; :158 `void (*d_prune)(struct dentry *)` |
| super ops | holds | fs.h:2340 `struct inode *(*alloc_inode)(struct super_block *)`; :2342 `void (*free_inode)(struct inode *)` (RCU-delayed); :2341 `destroy_inode`; :2347 `void (*evict_inode)(struct inode *)`; :2349 `int (*sync_fs)(struct super_block *, int wait)`; :2354 `int (*statfs)(struct dentry *, struct kstatfs *)`; :2345 `write_inode(inode, wbc)` |
| fs_context mount API | holds | fs.h:2673 `int (*init_fs_context)(struct fs_context *)`, :2674 `const struct fs_parameter_spec *parameters`; fs_parser.h:40 `struct fs_parameter_spec {name, type, opt, flags, data}`, `fsparam_u32` :120, `fsparam_string` :126, `fsparam_flag` :116, `fs_parse()` :71 (inline over `__fs_parse`); fs/super.c:1340 `int get_tree_nodev(struct fs_context *, int (*fill_super)(struct super_block *, struct fs_context *))` EXPORT_SYMBOL super.c:1346; `get_tree_keyed` super.c:1356 |
| aops still have `writepage` | absent | fs.h:439-480: only `read_folio, writepages, dirty_folio, readahead, write_begin, write_end, bmap, invalidate_folio, release_folio, free_folio, direct_IO, migrate_folio, launder_folio, is_partially_uptodate, is_dirty_writeback, error_remove_folio, swap_*` |
| Large folios | holds | include/linux/pagemap.h:456 `static inline void mapping_set_large_folios(struct address_space *)` (= order range 0..MAX_PAGECACHE_ORDER, :458); `mapping_set_folio_order_range` :419; netfs handles large folios natively |
| Multigrain timestamps | holds (opt-in) | fs.h:2670 `#define FS_MGTIME 64` in `file_system_type.fs_flags`; `is_mgtime()` fs.h:2701; `inode_set_ctime_current()` fs.h:1659 EXPORT_SYMBOL fs/inode.c:2784; `inode_set_ctime_deleg()` fs.h:1660 EXPORT_SYMBOL inode.c:2857; `current_time` EXPORT_SYMBOL inode.c:2271 |
| Inode time accessors (no direct `i_mtime`) | holds | split fields `i_mtime_sec`/`i_ctime_nsec` fs.h:711-715; accessors `inode_get_mtime` :1708, `inode_set_mtime` :1723, `inode_get_ctime` :1752, `inode_set_ctime_to_ts` :1760, `inode_set_atime` :1689 |
| RCU-walk rules | holds | vfs.rst:1281-1290: d_revalidate in `flags & LOOKUP_RCU` must not block or store to the dentry, may not trust d_parent/d_inode, return -ECHILD to retry in ref-walk; vfs.rst:643-649: `permission` in RCU walk when `mask & MAY_NOT_BLOCK`, return -ECHILD; get_link with NULL dentry returns ERR_PTR(-ECHILD) if it must block (vfs.rst:620-624) |

Consequence: `mkdir` must return a dentry pointer; `d_revalidate` uses the
supplied `dir`/`name`; dirty data is written only through `writepages`
(`netfs_writepages`).

## 3. Dcache/icache primitives

| Symbol | Verdict | Signature / export |
|---|---|---|
| `iget5_locked` | holds | fs.h:3308 `struct inode *iget5_locked(struct super_block *, unsigned long, int (*test)(struct inode *, void *), int (*set)(struct inode *, void *), void *)`; EXPORT_SYMBOL fs/inode.c:1366 |
| `ilookup5` | holds | fs.h:3300 `struct inode *ilookup5(struct super_block *, unsigned long hashval, test, void *data)`; EXPORT_SYMBOL inode.c:1620 (`ilookup5_nowait` :1586) |
| `unlock_new_inode` | holds | EXPORT_SYMBOL inode.c:1195; `iget_failed` fs/bad_inode.c:250 |
| `d_splice_alias` | holds | fs/dcache.c:2987 `struct dentry *d_splice_alias(struct inode *, struct dentry *)`; EXPORT_SYMBOL dcache.c:3036 |
| `d_alloc_parallel` | holds | dcache.c:2526 `struct dentry *d_alloc_parallel(struct dentry *parent, const struct qstr *name, wait_queue_head_t *wq)`; EXPORT_SYMBOL :2634 |
| `d_add` / `d_instantiate` / `d_instantiate_new` | holds | dcache.h:277, :235; EXPORT_SYMBOL dcache.c:2714, :1951, :1978 |
| `d_invalidate` / `d_drop` / `__d_drop` | holds | dcache.c:1643 `void d_invalidate(struct dentry *)` EXPORT_SYMBOL :1672; `d_drop` :605; `__d_drop` :579 |
| `d_prune_aliases` | holds | dcache.c:1093 `void d_prune_aliases(struct inode *)`; EXPORT_SYMBOL :1108 |
| `shrink_dcache_parent` | holds | dcache.c:1547; EXPORT_SYMBOL :1578 |
| `d_make_root` | holds | dcache.h:259; EXPORT_SYMBOL dcache.c:1993 |
| `dget_parent` | holds | dcache.h:349; EXPORT_SYMBOL dcache.c:987 |
| `take_dentry_name_snapshot` | holds | dcache.c:360 `void take_dentry_name_snapshot(struct name_snapshot *, struct dentry *)`; EXPORT_SYMBOL :387; release :398 |
| `d_hash_and_lookup` | not exported | (`d_lookup` EXPORT_SYMBOL dcache.c:2329 is the alternative: hash the qstr with `full_name_hash` first) |
| `invalidate_inode_pages2_range` | GPL-only | mm/truncate.c:722 EXPORT_SYMBOL_GPL; `int invalidate_inode_pages2_range(struct address_space *, pgoff_t start, pgoff_t end)` |
| `truncate_setsize` / `truncate_pagecache` | holds | include/linux/mm.h:2390/:2389; EXPORT_SYMBOL truncate.c:796, :772 |
| `setattr_prepare` / `setattr_copy` | holds | fs/attr.c:161 `int setattr_prepare(struct mnt_idmap *, struct dentry *, struct iattr *)` EXPORT_SYMBOL :226; attr.c:340 `void setattr_copy(struct mnt_idmap *, struct inode *, const struct iattr *)` EXPORT_SYMBOL :369 |
| `generic_permission` | holds | fs.h:3062 `int generic_permission(struct mnt_idmap *, struct inode *, int)`; EXPORT_SYMBOL fs/namei.c:508 |
| `inode_init_owner` | holds | fs.h:2065 `void inode_init_owner(struct mnt_idmap *, struct inode *, const struct inode *dir, umode_t)`; EXPORT_SYMBOL inode.c:2568 |
| `generic_fillattr` | holds | fs.h:3530 `void generic_fillattr(struct mnt_idmap *, u32 request_mask, struct inode *, struct kstat *)`; EXPORT_SYMBOL fs/stat.c:114 |
| `inode_newsize_ok` | holds | attr.c:244 `int inode_newsize_ok(const struct inode *, loff_t)`; EXPORT_SYMBOL :272 |
| `filemap_write_and_wait_range` / `file_write_and_wait_range` | holds | EXPORT_SYMBOL mm/filemap.c:707, :805 |
| `locks_lock_file_wait` | holds (inline) | include/linux/filelock.h:420 `static inline int locks_lock_file_wait(struct file *, struct file_lock *)` → `locks_lock_inode_wait` EXPORT_SYMBOL fs/locks.c:2118 |
| `generic_file_llseek` | holds | fs.h:3441; EXPORT_SYMBOL fs/read_write.c:252 |
| `dir_emit` | holds (inline) | fs.h:3918 `static inline bool dir_emit(struct dir_context *, const char *name, int namelen, u64 ino, unsigned type)` |
| `simple_statfs` | holds | fs.h:3589; EXPORT_SYMBOL fs/libfs.c:53 |
| `kmem_cache_create` | differs (macro) | include/linux/slab.h:430 `_Generic` macro → `__kmem_cache_create_args` EXPORT_SYMBOL mm/slab_common.c:359; `kmem_cache_alloc` macro slab.h:739 → `kmem_cache_alloc_noprof` EXPORT_SYMBOL mm/slub.c:4222; `kmem_cache_alloc_lru_noprof` (for `alloc_inode`) slub.c:4234; `kmem_cache_free` slub.c:4767; `kmem_cache_destroy` slab_common.c:545 |
| `mempool_*` | differs (macros over `_noprof`) | `mempool_create` macro mempool.h:59 → `mempool_create_node_noprof` EXPORT_SYMBOL mm/mempool.c:288; `mempool_alloc` macro :67 → `mempool_alloc_noprof` :451; `mempool_init_noprof` :250; `mempool_free` :550; `mempool_destroy` :193; `mempool_exit` :175 |
| Other: `iput`, `ihold`, `igrab`, `clear_inode`, `truncate_inode_pages_final`, `invalidate_mapping_pages`, `set_nlink`/`inc_nlink`/`drop_nlink`/`clear_nlink`, `kill_anon_super`, `register_filesystem`, `super_setup_bdi`, `d_move`, `d_find_alias`, `d_tmpfile`, `find_inode_rcu`, `inode_init_once`, `generic_file_open`, `generic_read_dir`, `inode_maybe_inc_iversion` | holds, EXPORT_SYMBOL | inode.c:1927,531,1556,770; truncate.c:497,579; inode.c:461,480,422,439; super.c:1286; filesystems.c:94; super.c:1888; dcache.c:2882,1062,3122; inode.c:1738,515; open.c:1630; libfs.c:231,2022 |

Consequence: the only VFS/mm symbol on the list that is GPL-only is
`invalidate_inode_pages2_range`; with a GPL module license it is usable. Use
macros (`kmem_cache_create`, `mempool_create`), not the `_noprof` names.

## 4. Virtio driver API

| Assumption | Verdict | Evidence |
|---|---|---|
| `virtio_find_vqs` + `struct virtqueue_info` | holds | include/linux/virtio_config.h:221 `int virtio_find_vqs(struct virtio_device *, unsigned int nvqs, struct virtqueue *vqs[], struct virtqueue_info vqs_info[], struct irq_affinity *desc)` (inline); :29 `struct virtqueue_info { const char *name; vq_callback_t *callback; bool ctx; }` |
| `virtqueue_add_sgs` | holds, GPL | drivers/virtio/virtio_ring.c:2300 `int virtqueue_add_sgs(struct virtqueue *, struct scatterlist *sgs[], unsigned int out_sgs, unsigned int in_sgs, void *data, gfp_t)`; EXPORT_SYMBOL_GPL :2319 |
| `virtqueue_kick_prepare` / `virtqueue_notify` / `virtqueue_kick` | holds, GPL | :2465 `bool virtqueue_kick_prepare(struct virtqueue *)` GPL :2472; :2482 `bool virtqueue_notify(struct virtqueue *)` GPL :2496; `virtqueue_kick` GPL :2516 |
| `virtqueue_get_buf` | holds, GPL | :2545 `void *virtqueue_get_buf(struct virtqueue *, unsigned int *len)`; GPL :2549 |
| `virtqueue_disable_cb` / `virtqueue_enable_cb` | holds, GPL | :2559 `void virtqueue_disable_cb(struct virtqueue *)` GPL :2568; :2627 `bool virtqueue_enable_cb(struct virtqueue *)` GPL :2633; `virtqueue_enable_cb_delayed` GPL :2658; `virtqueue_enable_cb_prepare` :2582 |
| `virtio_cread*` | holds (inline/macros) | virtio_config.h:394 `virtio_cread(vdev, structname, member, ptr)`, :458 `virtio_cread_le`, :566 `virtio_cread32`, :523 `virtio_cread_bytes` |
| `virtio_get_shm_region` | holds | virtio_config.h:329 `bool virtio_get_shm_region(struct virtio_device *, struct virtio_shm_region *, u8 id)` (inline over `config->get_shm_region`, :130) |
| `register_virtio_driver` | holds, GPL | include/linux/virtio.h:248 macro → `__register_virtio_driver(drv, THIS_MODULE)` :250, EXPORT_SYMBOL_GPL drivers/virtio/virtio.c:452; `unregister_virtio_driver` GPL :458; `virtio_reset_device` GPL :268; `virtio_check_driver_offered_feature` GPL :124 |
| freeze/restore hooks | holds (+ reset hooks) | virtio.h:238-242 `int (*freeze)(struct virtio_device *); int (*restore)(...); int (*reset_prepare)(...); int (*reset_done)(...); void (*shutdown)(...)` |
| virtio-mmio: one interrupt per device | holds: exactly one, shared | virtio_mmio.c:496 `platform_get_irq(vm_dev->pdev, 0)`, :502 `request_irq(irq, vm_interrupt, IRQF_SHARED, ...)`; `desc` (irq_affinity) ignored |
| virtio-mmio: multiple virtqueues | holds | vm_find_vqs loops `nvqs` (:513-527); vm_interrupt runs `vring_interrupt` on every vq under one spinlock (:317-321) |
| virtio-mmio: shared-memory regions | holds | `vm_get_shm_region` :536-560 (SHM_SEL/LEN/BASE regs), wired `.get_shm_region` :578 |
| Event-idx | holds | virtio_ring.c:2978 `VIRTIO_RING_F_EVENT_IDX` kept by `vring_transport_features` (:2970); also packed ring :2984, NOTIFICATION_DATA :2988 |
| Device ids | see below | include/uapi/linux/virtio_ids.h:32-70 |

IDs assigned in this header: 1-13, 16-41 (highest `VIRTIO_ID_GPIO 41`,
virtio_ids.h:70); 14 and 15 unlisted here; transitional 0x1000-0x1009.
`[INFERENCE]` the OASIS spec has since assigned ids above 41 and reserves
14/15, so none of these gaps is safe; pick an id far from the assigned range
(the mmio DeviceID register is 32-bit) and keep it private to vibevmm. A
virtio-pci modern device id is `0x1040 + id` and the spec range ends at
0x107f, so a PCI transport would cap ids at 63.

Consequences: every virtio core entry point is `EXPORT_SYMBOL_GPL`, so the
module must declare `MODULE_LICENSE("GPL")` (a proprietary license would fail
symbol resolution and also disable module tracepoints, kernel/tracepoint.c:524).
On virtio-mmio there is no per-queue interrupt or MSI affinity: multiple
request queues share one IRQ handler that scans all vqs, so queue-per-CPU only
spreads lock contention, not interrupts.

## 5. virtio-fs comparison (fs/fuse)

| Assumption | Verdict | Evidence |
|---|---|---|
| Server→guest notification queue exists | absent | fs/fuse/virtio_fs.c:40-43 queue types only `VQ_HIPRIO, VQ_REQUEST`; nvqs = `VQ_REQUEST + num_request_queues` (:952). No notify vq, so no FUSE_NOTIFY_* invalidation over virtio-fs |
| Completion runs in a workqueue | holds | vq callback `virtio_fs_vq_done` only does `schedule_work(&fsvq->done_work)` (:901-907); `virtio_fs_requests_done_work` (:813) drains `virtqueue_get_buf` with disable/enable_cb loop (:826-833), completes inline, or for `may_block` requests schedules a second per-request work item (:842-849) |
| Argument buffers copied | holds (non-page args only) | `copy_args_to_argbuf` kmalloc's one buffer and memcpy's every in-arg (:694-720); out-args memcpy'd back in `copy_args_from_argbuf` (:724-758); page/folio args (`in_pages`/`out_pages`) are mapped straight into the sg list (`sg_init_fuse_args` :1344, :1300-1316) |
| Request-queue count | holds: device-configured, multi-queue | `num_request_queues` read from config space (:944), capped by nr_cpu_ids (:950), CPU→queue map from vq affinity or `group_cpus_evenly` (:873-895) |
| FUSE passthrough on virtio-fs | absent in practice | `CONFIG_FUSE_PASSTHROUGH=y` (cfg:3278) but backing files are registered only via `FUSE_DEV_IOC_BACKING_OPEN` on /dev/fuse (fs/fuse/dev.c:2620); virtio_fs.c has no backing-file path; passthrough also refused with writeback cache (fs/fuse/inode.c:1420-1428) |
| DAX for virtio-fs | holds (config), needs device | `CONFIG_FUSE_DAX=y` cfg:3277; setup reads shm region `VIRTIO_FS_SHMCAP_ID_CACHE` (virtio_fs.c:1072) and calls `devm_memremap_pages` with `MEMORY_DEVICE_FS_DAX` (:1093-1106); mount option `dax=always|never|inode` (:103-117) |
| Writeback cache | holds, server-negotiated | `fc->writeback_cache` set from `FUSE_WRITEBACK_CACHE` in INIT reply (inode.c:1362-1363), requested by the client (:1488); affects attr handling (inode.c:323, :511) and short-read semantics (file.c:781-785) |
| FUSE-over-io_uring | on | cfg:3279 (irrelevant for virtio-fs) |

## 6. Rust

| Assumption | Verdict | Evidence |
|---|---|---|
| VFS/filesystem Rust abstractions exist | absent | `rust/kernel/fs.rs:7` only `pub mod file;` → `rust/kernel/fs/file.rs` (struct File / LocalFile wrappers). No inode, super_block, dentry, address_space or netfs bindings. Other modules present: alloc, auxiliary, block, configfs, cpu*, cred, device*, devres, dma, driver, drm, error, faux, firmware, fs, init, io, ioctl, kunit, list, miscdevice, mm, net, of, opp, page, pci, pid_namespace, platform, rbtree, revocable, security, seq_file, sync, task, time, tracepoint, types, uaccess, workqueue, xarray. No virtio bindings |
| `CONFIG_RUST=y` | absent | cfg: `CONFIG_RUST` and `CONFIG_RUST_IS_AVAILABLE` absent; `CONFIG_HAVE_RUST=y` cfg:728. RUST depends on RUST_IS_AVAILABLE (init/Kconfig:2035), which is `$(success,scripts/rust_is_available.sh)` (init/Kconfig:70-71): the build container has no rustc/bindgen |

Consequence: the module is C.

## 7. DAX feasibility

| Requirement | Verdict | Evidence |
|---|---|---|
| `devm_memremap_pages` | holds, GPL | mm/memremap.c:403 `void *devm_memremap_pages(struct device *, struct dev_pagemap *)`; EXPORT_SYMBOL_GPL :418 |
| `ZONE_DEVICE` | on | cfg:1082; needs MEMORY_HOTPLUG (cfg:1026), MEMORY_HOTREMOVE (cfg:1031), SPARSEMEM_VMEMMAP (cfg:1018), ARCH_HAS_PTE_DEVMAP (cfg:1078) (mm/Kconfig:1127-1132; arm64 selects PTE_DEVMAP arch/arm64/Kconfig:47) |
| `FS_DAX` / `FS_DAX_PMD` / `DAX` | on | cfg:3252, 3253, 3172 (fs/Kconfig:59-64, :91-95) |
| FUSE_DAX | on | cfg:3277 (fs/fuse/Kconfig:42-48 depends VIRTIO_FS, FS_DAX, DAX) |
| struct-page cost | holds | `devm_memremap_pages` builds a vmemmap for the whole window: 64-byte `struct page` per 4 KiB page (ARM64_4K_PAGES cfg:418) ≈ 1.56 % of window (16 MiB per GiB) `[INFERENCE: sizeof(struct page)=64 on arm64]`; FUSE maps the window in 2 MiB chunks (fs/fuse/dax.c:21-23) |
| VMM side | unverified | requires vibevmm to expose a virtio shm region on the device; outside this tree |

## 8. Module mechanics

| Assumption | Verdict | Evidence |
|---|---|---|
| Built with `make -C <src> O=<build> M=<moddir>` against the kernel build tree with `Module.symvers` | holds | vibevmm `kernel/build.sh` runs `make ... modules` only to have modpost write `Module.symvers` (build.sh:183-186); `nvidia.sh` dies without it (nvidia.sh:72) and builds with `SYSSRC=$ksrc SYSOUT=$kbuild ARCH CROSS_COMPILE LD OBJDUMP` (nvidia.sh:103-106; NVIDIA's makefile turns this into `make -C SYSSRC O=SYSOUT M=...`), then checks `vermagic=` starts with `kernel.release` (nvidia.sh:117-118). Build tree lives in the build container cache (`$cache/build/$KERNEL_TAG-aarch64`, build.sh:149), i.e. the `vmon-kernel-cache` volume |
| vermagic content | holds | include/linux/vermagic.h:41-47: `UTS_RELEASE " " SMP preempt mod_unload modversions MODULE_ARCH_VERMAGIC RANDSTRUCT`; arm64 `MODULE_ARCH_VERMAGIC "aarch64"` (arch/arm64/include/asm/vermagic.h:8). Here: SMP=y cfg:306, PREEMPT_NONE cfg:107 (no "preempt "), MODULE_UNLOAD cfg:828, MODVERSIONS off cfg:831, RANDSTRUCT_NONE cfg:3577 → **`6.16.9-vmon SMP mod_unload aarch64`** |
| vermagic compared exactly | holds | kernel/module/main.c:2567-2570 `!same_magic(...)` → -ENOEXEC; without MODVERSIONS `same_magic` is plain `strcmp` (kernel/module/internal.h:411-414). MODULE_FORCE_LOAD off (cfg:827) |
| Nothing else must match | differs | `.gnu.linkonce.this_module` size must equal the running `sizeof(struct module)` (main.c:2076-2079, :2766); and with no CRCs nothing checks struct layouts (inode, super_block, netfs_*), so the module must be built from the identical .config and source; compiler should be the same `gcc (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0` (cfg:5) because plugin/flag-dependent layout and `CC_VERSION_TEXT` checks in Kbuild |
| Unsigned module loads | holds | MODULE_SIG off (cfg:833), SECURITY_LOCKDOWN_LSM off (cfg:3532), LSM="landlock,bpf"; taints OOT only (main.c:2497); `MODPROBE_PATH=""` so no autoload (common.config) |
| Module tracepoints | holds | TRACEPOINTS=y cfg:272, EVENT_TRACING cfg:4151; OOT/unsigned taint allowed (kernel/tracepoint.c:524-528) |
| BTF for modules | absent | DEBUG_INFO_BTF_MODULES off (cfg:3970, pinned off in common.config) |

Debug options on arm64 (resolved value → availability):

| Option | Resolved | Available? |
|---|---|---|
| `KASAN` (generic) | off cfg:4053 | yes: HAVE_ARCH_KASAN cfg:4046, CC_HAS_KASAN_GENERIC cfg:4050 (lib/Kconfig.kasan:79-80) |
| `PROVE_LOCKING` | off cfg:4086 | yes: DEBUG_KERNEL cfg:3950, LOCK_DEBUGGING_SUPPORT cfg:4085 (lib/Kconfig.debug:1373-1375) |
| `KCSAN` | off cfg:4002 | yes but exclusive with KASAN: HAVE_ARCH_KCSAN cfg:4000 (arm64 needs EXPERT, cfg:230), HAVE_KCSAN_COMPILER cfg:4001; `depends on DEBUG_KERNEL && !KASAN` (lib/Kconfig.kcsan:16) |
| `DEBUG_ATOMIC_SLEEP` | off cfg:4094 | yes (lib/Kconfig.debug:1574-1577) |
| `DEBUG_KMEMLEAK` | off cfg:4031 | yes: HAVE_DEBUG_KMEMLEAK cfg:4030 (mm/Kconfig.debug:240-241) |
| `DEBUG_FS` | on cfg:3992 (ALLOW_ALL cfg:3993) | already on |
| `DEBUG_LIST` | on cfg:4110 | already on |
| `DEBUG_INFO` | on cfg:3956 | already on |
| `KFENCE`, `UBSAN`, `DEBUG_VM`, `DEBUG_OBJECTS` | off cfg:4055, 3999, 4039, 4033 | yes |

Consequence: a debug flavour changes `struct module`, lock and struct layouts,
so it is a second kernel build and a second `ovnfs.ko` build; vermagic alone
would not catch the mismatch (`PROVE_LOCKING` etc. are not in vermagic).

## 9. Reference implementations

| Topic | Location |
|---|---|
| Sorted-directory binary search | fs/erofs/namei.c:16 `erofs_dirnamecmp`, :45 `find_target_dirent` (in-block bsearch), :89 `erofs_find_target_block` (block-level bsearch), :161 `erofs_namei`, :193 `erofs_lookup` |
| Directory completeness + d_prune | fs/ceph/super.h:682 `__ceph_dir_set_complete`, :696 `__ceph_dir_clear_complete`, :706/:712 `__ceph_dir_is_complete[_ordered]`; fs/ceph/dir.c:186 `__dcache_readdir` (readdir from dcache when complete), :317 `ceph_readdir`, :1837 `dentry_lease_is_valid`, :1914 `dir_lease_is_valid`, :1949 `ceph_d_revalidate`, :2094 `ceph_d_prune` (clears completeness when a child is pruned); fs/ceph/inode.c:1957 `ceph_readdir_prepopulate`, :1911 `fill_readdir_cache`, :1564 `ceph_fill_trace`, :985 `ceph_fill_inode` |
| Readdirplus dcache priming | fs/nfs/dir.c:726 `nfs_prime_dcache`, :807 `nfs_readdir_entry_decode` (calls it :818), :663 `nfs_use_readdirplus`, :930 `nfs_readdir_xdr_to_array`, :1223 `nfs_readdir`, :1408 `nfs_verify_change_attribute`, :1678/:1805 `nfs_lookup_revalidate[_dentry]`, :1874 `nfs_dentry_delete`, :1914 `nfs_dentry_iput` |
| Silly rename | fs/nfs/dir.c:2534 `nfs_unlink` (→ :2548), :2713 `nfs_rename` (→ :2762); fs/nfs/unlink.c:443 `nfs_sillyrename`, :121 `nfs_call_unlink`, :63 `nfs_async_unlink_release` |
| Delegation grant/recall | fs/nfs/delegation.c:442 `nfs_inode_set_delegation`, :231 `nfs_inode_reclaim_delegation`, :270 `nfs_do_return_delegation`, :552 `nfs_end_delegation_return`, :768 `nfs4_inode_return_delegation`, :1130 `nfs_async_inode_return_delegation` (CB_RECALL path), :936 `nfs_expire_all_delegations`, :1073 `nfs_remove_bad_delegation` |
| FUSE `attr_version` race guard | fs/fuse/fuse_i.h:926 `atomic64_t attr_version` (conn), :141 per-inode, :1050 `fuse_get_attr_version`; fs/fuse/inode.c:358 drop stale attrs `if (attr_version != 0 && fi->attr_version > attr_version)`, :249/:571 bump on local change; fs/fuse/dir.c:236/:384 snapshot before LOOKUP, :2049/:2075 setattr guard, dir.c:997 bump |

## 10. NFS client baseline

| Option | Resolved | Evidence |
|---|---|---|
| `NFS_V3` | y | cfg:3398 (+ V3_ACL cfg:3399) |
| `NFS_V4` | y | cfg:3400 |
| `NFS_V4_1` | y | cfg:3402 (PNFS_FILE_LAYOUT cfg:3404) |
| `NFS_V4_2` | y | cfg:3403 |
| `NFS_FSCACHE` | n | cfg:3411 (FSCACHE cfg:3294 off): NFS page cache is the plain one, no netfs |
| UDP | disabled | NFS_DISABLE_UDP_SUPPORT=y cfg:3414 |
| rsize/wsize | default = server's `rtpref`/`wtpref`, capped by `rtmax`/`wtmax`, RPC payload, and 1 MiB | fs/nfs/client.c:833-853; include/linux/nfs_xdr.h:14 `NFS_MAX_FILE_IO_SIZE (1048576U)`, :15 default 4096 when unset; rounding fs/nfs/internal.h:805-826 |
| NFSv4.0 delegations need a reachable callback | holds → none behind NAT | v4.0 SETCLIENTID advertises `cb_client4` = netid tcp/tcp6 + `clp->cl_ipaddr.port` (fs/nfs/nfs4proc.c:6659-6667, :6593-6598); `cl_ipaddr` defaults to `rpc_localaddr` of the mount connection (fs/nfs/nfs4client.c:248-261), i.e. guest 10.0.2.15, which the host cannot dial through user-mode NAT. Server-side CB_NULL fails → server grants no delegations `[INFERENCE: RFC 7530 server behaviour; also depends on whether ovnfs's v4.0 server grants delegations at all]`. v4.1+ would use the fore-channel backchannel (SUNRPC_BACKCHANNEL cfg:3425) but the server is v4.0 only |

Consequence: the NFSv4.0 baseline measures close-to-open + attribute-cache
revalidation without delegations; NFSv3 likewise has no leases.

## Symbols not exported or GPL-only

Symbols the module needs that are not exported or are GPL-only.

Not exported (use the listed alternative):
- `d_hash_and_lookup` → `d_lookup` with a pre-hashed qstr.
- `dir_emit`, `locks_lock_file_wait`, `virtio_find_vqs`, `virtio_cread*`,
  `virtio_get_shm_region`, `netfs_inode_init`, `netfs_inode`,
  `mapping_set_large_folios`, inode time accessors: static inline (no export
  needed; their callees are exported).
- `kmem_cache_create`, `kmem_cache_alloc`, `mempool_create`, `mempool_alloc`:
  macros over exported `__kmem_cache_create_args`,
  `kmem_cache_alloc[_lru]_noprof`, `mempool_create_node_noprof`,
  `mempool_alloc_noprof`.

GPL-only (require `MODULE_LICENSE("GPL")`):
- virtio: `__register_virtio_driver`, `unregister_virtio_driver`,
  `virtqueue_add_sgs`, `virtqueue_kick_prepare`, `virtqueue_notify`,
  `virtqueue_kick`, `virtqueue_get_buf`, `virtqueue_disable_cb`,
  `virtqueue_enable_cb`, `virtqueue_enable_cb_delayed`,
  `virtqueue_get_vring_size`, `virtio_reset_device`,
  `virtio_check_driver_offered_feature`.
- mm: `invalidate_inode_pages2_range`; `devm_memremap_pages` (DAX only).
- netfs: `netfs_extract_user_iter` (only if the module does its own DIO
  extraction).

Everything else listed in sections 1-3 is plain `EXPORT_SYMBOL`.

## Kernel config for the module

Options for vibevmm's `kernel/config/common.config`.

Module (production flavour), with current resolved value:
- `CONFIG_NETFS_SUPPORT=y` — currently `y` (cfg:3291), but only because
  `CONFIG_CIFS=y` selects it; promptless, so the line is a tripwire for
  `build.sh`'s `verify_config`, and the comment must say CIFS is what keeps it
  on.
- Already pinned and required, keep: `CONFIG_MODULES=y` (y),
  `CONFIG_MODULE_UNLOAD=y` (y), `# CONFIG_MODVERSIONS is not set` (n),
  `# CONFIG_MODULE_SIG is not set` (n), `CONFIG_VIRTIO_MMIO=y` (y),
  `CONFIG_CIFS=y` (y).
- Optional: `CONFIG_NETFS_STATS=y` — currently n (cfg:3292); gives
  `/proc/fs/netfs/stats` for benchmarking.
- Not needed: FSCACHE (n), RUST (cannot be enabled: no toolchain), FUSE_DAX /
  ZONE_DEVICE / FS_DAX already y.

Debug flavour (separate fragment, separate kernel + module build), current
resolved value in brackets:
- `CONFIG_KASAN=y` [n], `CONFIG_KASAN_GENERIC=y` [absent],
  `CONFIG_KASAN_INLINE=y` or OUTLINE [absent]
- `CONFIG_PROVE_LOCKING=y` [n] (selects LOCKDEP, DEBUG_SPINLOCK [n],
  DEBUG_MUTEXES [n], DEBUG_LOCK_ALLOC [n])
- `CONFIG_DEBUG_ATOMIC_SLEEP=y` [n]
- `CONFIG_DEBUG_KMEMLEAK=y` [n]
- `CONFIG_DEBUG_FS=y` [y, already]
- `CONFIG_DEBUG_LIST=y` [y, already]
- `CONFIG_NETFS_DEBUG=y` [n] (netfs `_enter/_debug` tracing)
- `CONFIG_DEBUG_VM=y` [n] (folio/page-cache assertions)
- `CONFIG_DEBUG_OBJECTS=y`, `CONFIG_DEBUG_OBJECTS_WORK=y` [n]
- `CONFIG_TRACEPOINTS` [y, already]; `CONFIG_FTRACE=y` [y]
- `CONFIG_KCSAN=y` [n] only in a third flavour: excludes KASAN
  (lib/Kconfig.kcsan:16).
