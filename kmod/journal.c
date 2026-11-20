// SPDX-License-Identifier: GPL-2.0-only
/*
 * journal.c - namespace operations and closed-file data sent behind the caller.
 *
 * A journaled syscall changes the dcache, the directory tables and the
 * inodes exactly as a successful reply would, appends an entry and returns.
 * One worker per mount takes entries oldest first, packs them into COMPOUND
 * calls (one at a time) and applies the replies in order. Entries:
 *   CREATE, MKDIR  of a name known to be absent, with an id from the pool
 *                  ALLOC_IDS fills, so the guest knows the id in advance;
 *   UNLINK, LINK, RENAME  of names known, files' link counts current, a
 *                  replaced directory known to be empty;
 *   RMDIR          of a directory known to be empty (complete, empty table);
 *   SETATTR        mode, mtime and size of a file or directory whose
 *                  attributes are current (owner changes stay synchronous);
 *   DATA           a closed file's dirty pages (WRITE runs, then a SETATTR
 *                  with the local mtime), queued once per file at close.
 * A CREATE whose file is closed, small and wholly in the page cache carries
 * the content and consumes the file's DATA entry: one sub-request per file.
 * A size SETATTR of such a file rewritten after its truncation carries the
 * whole content as one WRITE and sets the final size and mtime.
 *
 * Ordering rules:
 *  1. The daemon receives the journal in append order, each entry once: one
 *     batch is in flight at a time and its sub-requests run in order. So it
 *     sees a sequence the syscalls could have produced one by one. A file
 *     has data in a batch at most once (OVNFS_I_IN_BATCH): a later entry
 *     with data of the same file starts the next batch, since taking its
 *     folios would wait for the writeback of the earlier ones.
 *  2. Data never overtakes its file's CREATE or a size change (hold_seq):
 *     a truncation moves the file's queued DATA entry behind it, so data
 *     written before it cannot reappear after it; data taken for an entry
 *     that a later size change overtook is given back and queued again
 *     behind it. Reads never wait for the journal (they may hold folio locks
 *     a truncation waits for while its entry is held): a truncation lowers
 *     the zero point to where it cuts, uncached pages from there on read as
 *     zeros, and the zero point rises past written data only when its
 *     writeback ends, after the daemon executed it and the truncation it
 *     follows (the truncation lowers it only after waiting for writeback
 *     in flight). writepages, which is
 *     not ordered with the journal, leaves a file with any unanswered entry
 *     alone (background, and in the worker) or waits for them (integrity):
 *     its WRITE and mtime SETATTR must not overtake a pending CREATE, size,
 *     mode or mtime change (integrity writeback waits only for the entries
 *     there when it starts, so a file changed in a loop cannot starve it).
 *     Conversely, while a writepages call for a file runs (wb_active), the
 *     worker sends none of the file's data or SETATTRs and no size change
 *     of it is appended: the two travel on different ports, and a
 *     truncation or an mtime that overtook a WRITE in flight, or a later
 *     truncation executed before it, would undo or cut its data.
 *  3. Barrier: a synchronous call waits until every journal entry naming one
 *     of the inodes it names has been answered (jseq <= jdone):
 *       - LOOKUP and READDIR of a directory: every entry that changes a name
 *         in it names it, so the daemon's answer includes them;
 *       - SETATTR and FSYNC of an inode: its creation, data and links;
 *       - synchronous namespace mutations (SYMLINK, MKNOD, and CREATE,
 *         MKDIR, UNLINK, RMDIR, RENAME, LINK when not journaled): every
 *         directory and node they name, so the daemon finds the names, ids
 *         and emptiness the guest saw;
 *       - sync, syncfs, unmount and remount read-only drain everything.
 *     Pending entries elsewhere (below a renamed directory, say) need no
 *     wait: the daemon resolves ids atomically against renames.
 *     Exempt, and why that is safe:
 *       - GETATTR of an inode with pending entries is not sent: its cached
 *         attributes are what the entries will produce, and refreshes are
 *         not applied to it while entries are pending;
 *       - READ: uncached pages were never dirty (dirty pages stay cached
 *         until written), so no pending entry carries them; pages past the
 *         zero point (a file created here, a range truncated here) read as
 *         zeros without a call, and a pending size change only affects
 *         bytes past it (rule 2);
 *       - WRITE from writepages: needs only rule 2;
 *       - READLINK (symlinks are never created through the journal),
 *         STATFS, POLL, ALLOC_IDS.
 *  4. A file's last name removed (unlink or rename over it, link count 1,
 *     not open) cancels its queued DATA entry instead of sending data the
 *     daemon is about to discard. Other cancellations are not done: an
 *     entry is sent even if a later one undoes it.
 *  5. Timestamps: a file's mtime travels with its CREATE, with the SETATTR
 *     after its WRITEs and with SETATTRs that set it or its size, and is
 *     always the mtime the guest shows when the sub-request is packed, not
 *     the one of the syscall that queued it. An entry may carry data written
 *     after later entries were queued; with the mtime of packing time, every
 *     later sub-request carries an mtime at least that recent, so the last
 *     one the daemon executes is the guest's. Replies to journal entries
 *     never move a node's times (OVNFS_ATTR_DATA). Attributes in
 *     a reply are applied only when no later entry names the inode (the
 *     daemon has not seen it yet: a pending LINK, say) and no change of
 *     this guest overtook the call; replies to writepages' WRITEs follow
 *     the same rule (ovnfs_apply_written). Otherwise only the size and
 *     mtime the daemon reports are noted.
 *
 * Late failures: an entry the daemon refuses cannot be reported to its
 * caller. It is logged (rate-limited), recorded in sb->s_wb_err (syncfs)
 * and in the mappings of the files involved (fsync, close), and the guest's
 * view converges on the daemon's: the directories involved lose their
 * complete listing, the names' dentries and the inodes' attributes are
 * revalidated on next use, and a node whose CREATE/MKDIR failed is marked
 * bad (calls on it fail with ESTALE, its dentries are dropped). Entries that
 * depended on a failed one fail on the daemon by themselves. An entry whose
 * reply never arrived (transport failure) is treated the same way, except
 * that its node is not marked bad: revalidation finds it if it was created.
 *
 * Locks: jlock is a spinlock, innermost, held only to link and unlink
 * entries and to take ids. The worker holds no VFS lock, no dir_lock and at
 * most one folio lock (of a dirty folio, never across a call), so callers
 * may wait for it under i_rwsem, dir_lock and the lock of a folio that is
 * not dirty (reads, which lock folios a truncation waits for, never wait
 * for it at all). The worker must never wait for the journal itself, nor
 * for anything that waits for it: it drops entries' inode references, which
 * may evict an unlinked inode and wait for its writeback, only after
 * publishing jdone; writepages of a file whose data is dropped never waits
 * for the journal; and the worker waits for a writepages call (wb_active)
 * only with an empty batch, while writepages waits for the journal only
 * before it counts itself in wb_active.
 */
#include <linux/fs.h>
#include <linux/highmem.h>
#include <linux/list.h>
#include <linux/pagemap.h>
#include <linux/sched.h>
#include <linux/slab.h>
#include <linux/unaligned.h>
#include <linux/vmalloc.h>
#include <linux/wait.h>
#include <linux/writeback.h>

#include "ovnfs.h"

/*
 * Bounds of the journal: a burst of tens of thousands of small creates (an
 * entry each, plus one per written file) fits without waiting.
 */
#define OVNFS_J_MAX_ENTRIES	(1U << 17)
#define OVNFS_J_MAX_BYTES	(32U << 20)
/* Ids asked for at once, and the stock below which the pool is refilled. */
#define OVNFS_ID_BATCH		8192
#define OVNFS_ID_LOW		2048

/* Largest file whose data is copied into a batch; bigger ones use writepages. */
#define OVNFS_SMALL		(256U << 10)
#define OVNFS_SMALL_PAGES	(OVNFS_SMALL / PAGE_SIZE)

/*
 * Batch buffers: vmapped chunks of 2^OVNFS_CHUNK_ORDER pages, so a 1 MiB
 * request takes 16 segments; with order-0 pages (fragmented memory) the
 * buffers shrink to stay within OVNFS_MAX_SEGS.
 */
#define OVNFS_CHUNK_ORDER	4
#define OVNFS_BATCH_REQ		(1U << 20)
#define OVNFS_BATCH_REP		(256U << 10)
#define OVNFS_BATCH_REQ_PAGES	(512U << 10)
#define OVNFS_BATCH_REP_PAGES	(128U << 10)

/* A sub-request's length prefix and header, and the largest sub-reply. */
#define OVNFS_SUB_HDR		(sizeof(__le32) + sizeof(struct ovnfs_req))
#define OVNFS_SUB_REP_MAX	(sizeof(__le32) + sizeof(struct ovnfs_rep) + sizeof(struct ovnfs_attr))
#define OVNFS_BATCH_MAX_SUBS	(OVNFS_BATCH_REP / OVNFS_SUB_REP_MAX)
#define OVNFS_BATCH_MAX_FOLIOS	(OVNFS_BATCH_REQ / PAGE_SIZE + OVNFS_BATCH_MAX_SUBS)
/* Request bytes of one small file's data: content, WRITE headers, mtime SETATTR. */
#define OVNFS_DATA_REQ_MAX	(OVNFS_SMALL + OVNFS_SMALL_PAGES * \
				 (OVNFS_SUB_HDR + sizeof(struct ovnfs_write_req)) + \
				 OVNFS_SUB_HDR + sizeof(struct ovnfs_setattr_req))

static_assert(OVNFS_BATCH_REQ / (PAGE_SIZE << OVNFS_CHUNK_ORDER) +
	      OVNFS_BATCH_REP / (PAGE_SIZE << OVNFS_CHUNK_ORDER) + 2 <= OVNFS_MAX_SEGS);
static_assert(OVNFS_BATCH_REQ_PAGES / PAGE_SIZE + OVNFS_BATCH_REP_PAGES / PAGE_SIZE + 2 <=
	      OVNFS_MAX_SEGS);
static_assert(OVNFS_BATCH_REQ + sizeof(struct ovnfs_req) <= OVNFS_MAX_FRAME);
static_assert(OVNFS_BATCH_REP + sizeof(struct ovnfs_rep) <= OVNFS_MAX_FRAME);
static_assert(OVNFS_SMALL <= OVNFS_MAX_IO);
/* Any one entry fits an empty batch, even with the smaller buffers. */
static_assert(OVNFS_DATA_REQ_MAX + OVNFS_SUB_HDR + sizeof(struct ovnfs_create_req) +
	      2 * OVNFS_NAME_MAX <= OVNFS_BATCH_REQ_PAGES);
static_assert(OVNFS_C_JOURNAL + OVNFS_J_NR == OVNFS_C_CREATE_DATA);

/* What became of a write-back sub-request; a file takes the worst of its own. */
enum ovnfs_sub_outcome {
	OVNFS_SUB_DONE,		/* executed, successfully or not */
	OVNFS_SUB_ALONE,	/* refused unexecuted for size: queue the data again */
	OVNFS_SUB_UNKNOWN,	/* maybe not executed: leave the data dirty */
};

/* A journal entry. Names follow the struct: name, then name2. */
struct ovnfs_jent {
	struct list_head node;	/* on jlist until the worker takes it */
	u64 seq;
	enum ovnfs_jop op;
	u32 bytes;		/* memory accounted in jbytes */
	struct inode *dir;	/* references held until the entry is freed */
	struct inode *dir2;
	struct inode *inode;
	struct inode *target;
	u32 uid, gid;		/* the caller's, for the request header */
	u32 mode;
	u32 flags;
	s64 mtime;
	u32 valid;		/* SETATTR */
	u64 size;		/* SETATTR */
	u16 len, len2;
	/* Worker state while in a batch. */
	enum ovnfs_sub_outcome outcome;	/* DATA */
	s64 sent_mtime;		/* DATA, content CREATE: the mtime sent */
	u32 first, nr;		/* content CREATE: its folios in the batch */
	unsigned int mtime_unsent;	/* DATA: the SETATTR did not run */
	unsigned int content;	/* CREATE: carries the file's content */
	unsigned int merged;	/* size SETATTR: sent with the file's whole content */
	unsigned int requeue;	/* DATA: dirty data was left for a later entry */
	unsigned int ready;	/* its syscall finished its local changes: it may be packed */
	char names[];
};

/* One sub-request of a batch. */
struct ovnfs_bsub {
	struct ovnfs_jent *ent;
	u16 op;			/* OVNFS_OP_* */
	u32 hdr;		/* offset of its length prefix in the request buffer */
	u64 off;		/* WRITE: file offset */
	u32 len;		/* WRITE: data bytes */
	u32 first, nr;		/* WRITE: its folios */
};

/* A physically chunked, virtually contiguous buffer. */
struct ovnfs_vbuf {
	void *addr;
	struct page **pages;	/* first page of every chunk */
	unsigned int chunks;
	unsigned int order;
};

/* The worker's batch; used by the worker only. */
struct ovnfs_batch {
	struct ovnfs_vbuf reqbuf, repbuf;
	void *req, *rep;
	u32 req_cap, nsubs_cap;
	u32 req_len;
	u32 nsubs, nents, nfolios;
	struct ovnfs_bsub subs[OVNFS_BATCH_MAX_SUBS];
	struct ovnfs_jent *ents[OVNFS_BATCH_MAX_SUBS];
	struct folio *folios[OVNFS_BATCH_MAX_FOLIOS];
};

static const char *const ovnfs_jop_names[OVNFS_J_NR] = {
	[OVNFS_J_CREATE] = "create",
	[OVNFS_J_MKDIR] = "mkdir",
	[OVNFS_J_UNLINK] = "unlink",
	[OVNFS_J_RENAME] = "rename",
	[OVNFS_J_LINK] = "link",
	[OVNFS_J_RMDIR] = "rmdir",
	[OVNFS_J_SETATTR] = "setattr",
	[OVNFS_J_DATA] = "write",
};

/* Frees a chunked buffer. */
static void ovnfs_vbuf_free(struct ovnfs_vbuf *v)
{
	unsigned int i;

	if (v->addr)
		vunmap(v->addr);
	for (i = 0; i < v->chunks; i++)
		__free_pages(v->pages[i], v->order);
	kfree(v->pages);
	memset(v, 0, sizeof(*v));
}

/* Allocates bytes in chunks of 2^order pages and maps them contiguously. */
static int ovnfs_vbuf_alloc(struct ovnfs_vbuf *v, size_t bytes, unsigned int order)
{
	unsigned int per = 1U << order, npages = bytes >> PAGE_SHIFT;
	struct page **map;
	unsigned int i, j;

	v->order = order;
	v->pages = kcalloc(npages / per, sizeof(*v->pages), GFP_KERNEL);
	map = kvcalloc(npages, sizeof(*map), GFP_KERNEL);
	if (!v->pages || !map)
		goto fail;
	for (i = 0; i < npages / per; i++) {
		v->pages[i] = alloc_pages(GFP_KERNEL | __GFP_NOWARN | __GFP_NORETRY, order);
		if (!v->pages[i])
			goto fail;
		v->chunks++;
		for (j = 0; j < per; j++)
			map[i * per + j] = v->pages[i] + j;
	}
	v->addr = vmap(map, npages, VM_MAP, PAGE_KERNEL);
	if (!v->addr)
		goto fail;
	kvfree(map);
	return 0;
fail:
	kvfree(map);
	ovnfs_vbuf_free(v);
	return -ENOMEM;
}

/* Allocates the worker's batch: large chunks, else order-0 pages and smaller buffers. */
static struct ovnfs_batch *ovnfs_batch_alloc(void)
{
	struct ovnfs_batch *b = kvzalloc(sizeof(*b), GFP_KERNEL);
	u32 rep;

	if (!b)
		return NULL;
	rep = OVNFS_BATCH_REP;
	b->req_cap = OVNFS_BATCH_REQ;
	if (ovnfs_vbuf_alloc(&b->reqbuf, OVNFS_BATCH_REQ, OVNFS_CHUNK_ORDER) ||
	    ovnfs_vbuf_alloc(&b->repbuf, OVNFS_BATCH_REP, OVNFS_CHUNK_ORDER)) {
		ovnfs_vbuf_free(&b->reqbuf);
		rep = OVNFS_BATCH_REP_PAGES;
		b->req_cap = OVNFS_BATCH_REQ_PAGES;
		if (ovnfs_vbuf_alloc(&b->reqbuf, OVNFS_BATCH_REQ_PAGES, 0) ||
		    ovnfs_vbuf_alloc(&b->repbuf, OVNFS_BATCH_REP_PAGES, 0)) {
			ovnfs_vbuf_free(&b->reqbuf);
			kvfree(b);
			return NULL;
		}
	}
	b->req = b->reqbuf.addr;
	b->rep = b->repbuf.addr;
	b->nsubs_cap = rep / OVNFS_SUB_REP_MAX;
	return b;
}

/* Frees the worker's batch. */
static void ovnfs_batch_free(struct ovnfs_batch *b)
{
	if (!b)
		return;
	ovnfs_vbuf_free(&b->reqbuf);
	ovnfs_vbuf_free(&b->repbuf);
	kvfree(b);
}

/* Says whether the journal has room for one more entry; jlock not needed. */
static bool ovnfs_journal_room(struct ovnfs_sb *sbi)
{
	return READ_ONCE(sbi->jcount) < OVNFS_J_MAX_ENTRIES &&
	       READ_ONCE(sbi->jbytes) < OVNFS_J_MAX_BYTES;
}

/* Starts the worker. */
void ovnfs_journal_kick(struct ovnfs_sb *sbi)
{
	queue_work(system_unbound_wq, &sbi->jwork);
}

/*
 * Allocates an entry with its names, waiting while the journal is full
 * (except in the worker, which frees the room). The caller fills it with
 * ovnfs_journal_commit or frees it with ovnfs_journal_abort.
 */
struct ovnfs_jent *ovnfs_journal_prepare(struct ovnfs_sb *sbi, const struct qstr *name,
					 const struct qstr *name2)
{
	u16 len = name ? name->len : 0, len2 = name2 ? name2->len : 0;
	struct ovnfs_jent *e;

	if (!ovnfs_journal_room(sbi) && current_work() != &sbi->jwork) {
		ovnfs_journal_kick(sbi);
		if (wait_event_killable(sbi->jwait, ovnfs_journal_room(sbi)))
			return ERR_PTR(-EINTR);
	}
	e = kzalloc(struct_size(e, names, len + len2), GFP_KERNEL);
	if (!e)
		return ERR_PTR(-ENOMEM);
	INIT_LIST_HEAD(&e->node);
	e->bytes = struct_size(e, names, len + len2);
	e->len = len;
	e->len2 = len2;
	if (len)
		memcpy(e->names, name->name, len);
	if (len2)
		memcpy(e->names + len, name2->name, len2);
	return e;
}

/* Frees an entry that was prepared but not committed. */
void ovnfs_journal_abort(struct ovnfs_sb *sbi, struct ovnfs_jent *e)
{
	kfree(e);
}

/* Takes a reference for an entry and marks the inode as named by entry seq. */
static struct inode *ovnfs_jent_hold(struct inode *inode, u64 seq)
{
	if (!inode)
		return NULL;
	ihold(inode);
	WRITE_ONCE(OVNFS_I(inode)->jseq, seq);
	return inode;
}

/* Appends a filled entry; jlock held. */
static void ovnfs_journal_append(struct ovnfs_sb *sbi, struct ovnfs_jent *e)
{
	e->seq = ++sbi->jtail;
	e->dir = ovnfs_jent_hold(e->dir, e->seq);
	e->dir2 = ovnfs_jent_hold(e->dir2, e->seq);
	e->inode = ovnfs_jent_hold(e->inode, e->seq);
	e->target = ovnfs_jent_hold(e->target, e->seq);
	if (e->op == OVNFS_J_CREATE || (e->op == OVNFS_J_SETATTR && (e->valid & OVNFS_SET_SIZE)))
		WRITE_ONCE(OVNFS_I(e->inode)->hold_seq, e->seq);
	list_add_tail(&e->node, &sbi->jlist);
	sbi->jcount++;
	sbi->jbytes += e->bytes;
	ovnfs_note_journal_high(sbi->jcount);
	ovnfs_count(OVNFS_C_JOURNAL + e->op, 1);
}

/* Says whether a writepages call is sending the file's data. */
static bool ovnfs_writeback_active(struct ovnfs_sb *sbi, struct inode *inode)
{
	bool active;

	spin_lock(&sbi->jlock);
	active = OVNFS_I(inode)->wb_active;
	spin_unlock(&sbi->jlock);
	return active;
}

/*
 * Appends a prepared entry that the worker does not pack until
 * ovnfs_journal_release: for an operation whose local change follows the
 * append (so replies to earlier entries no longer apply attributes to the
 * inode) and whose packing reads the inode's state.
 */
void ovnfs_journal_commit_held(struct ovnfs_sb *sbi, struct ovnfs_jent *e,
			       const struct ovnfs_jdesc *d)
{
	e->op = d->op;
	e->dir = d->dir;
	e->dir2 = d->dir2;
	e->inode = d->inode;
	e->target = d->target;
	e->mode = d->mode;
	e->flags = d->flags;
	e->mtime = d->mtime;
	e->valid = d->valid;
	e->size = d->size;
	e->uid = from_kuid_munged(&init_user_ns, current_fsuid());
	e->gid = from_kgid_munged(&init_user_ns, current_fsgid());
	spin_lock(&sbi->jlock);
	/* A size change waits for a writepages call of the file in progress. */
	while (e->op == OVNFS_J_SETATTR && (e->valid & OVNFS_SET_SIZE) &&
	       OVNFS_I(e->inode)->wb_active) {
		spin_unlock(&sbi->jlock);
		wait_event(sbi->jwait, !ovnfs_writeback_active(sbi, e->inode));
		spin_lock(&sbi->jlock);
	}
	ovnfs_journal_append(sbi, e);
	spin_unlock(&sbi->jlock);
}

/* Lets the worker pack an entry appended by ovnfs_journal_commit_held. */
void ovnfs_journal_release(struct ovnfs_sb *sbi, struct ovnfs_jent *e)
{
	/* Release: the worker that sees ready sees the local change. */
	smp_store_release(&e->ready, 1);
	ovnfs_journal_kick(sbi);
}

/*
 * Appends a prepared namespace entry. Called after the caller applied the
 * operation to its own caches but before it publishes the result (dentry
 * instantiation), so anything that can see the result also sees the
 * pending entry.
 */
void ovnfs_journal_commit(struct ovnfs_sb *sbi, struct ovnfs_jent *e, const struct ovnfs_jdesc *d)
{
	ovnfs_journal_commit_held(sbi, e, d);
	ovnfs_journal_release(sbi, e);
}

/*
 * Queues a closed file's dirty data. Returns an error (nothing queued) when
 * no entry could be allocated; the caller writes back synchronously then.
 */
int ovnfs_journal_data(struct inode *inode)
{
	struct ovnfs_sb *sbi = OVNFS_SB(inode->i_sb);
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_jent *e;

	if (!inode->i_nlink || test_bit(OVNFS_I_ORPHAN, &oi->flags) ||
	    test_bit(OVNFS_I_BAD, &oi->flags) || READ_ONCE(oi->data_ent))
		return 0;
	e = ovnfs_journal_prepare(sbi, NULL, NULL);
	if (IS_ERR(e))
		return PTR_ERR(e);
	e->op = OVNFS_J_DATA;
	e->ready = 1;
	e->inode = inode;
	e->uid = i_uid_read(inode);
	e->gid = i_gid_read(inode);
	spin_lock(&sbi->jlock);
	if (oi->data_ent) {
		spin_unlock(&sbi->jlock);
		kfree(e);
		return 0;
	}
	ovnfs_journal_append(sbi, e);
	oi->data_ent = e;
	spin_unlock(&sbi->jlock);
	ovnfs_journal_kick(sbi);
	return 0;
}

/* Drops an entry's references and accounting; no lock held. */
static void ovnfs_jent_free(struct ovnfs_sb *sbi, struct ovnfs_jent *e)
{
	spin_lock(&sbi->jlock);
	sbi->jcount--;
	sbi->jbytes -= e->bytes;
	spin_unlock(&sbi->jlock);
	iput(e->dir);
	iput(e->dir2);
	iput(e->inode);
	iput(e->target);
	kfree(e);
}

/*
 * Cancels a file's queued DATA entry, for a file whose last name is being
 * removed. Returns whether one was queued, so a failed removal can queue it
 * again. Must not drop the last inode reference: the caller holds one.
 */
bool ovnfs_journal_drop_data(struct inode *inode)
{
	struct ovnfs_sb *sbi = OVNFS_SB(inode->i_sb);
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_jent *e;

	if (!READ_ONCE(oi->data_ent))
		return false;
	spin_lock(&sbi->jlock);
	e = oi->data_ent;
	if (e) {
		list_del_init(&e->node);
		oi->data_ent = NULL;
	}
	spin_unlock(&sbi->jlock);
	if (!e)
		return false;
	ovnfs_jent_free(sbi, e);
	wake_up_all(&sbi->jwait);
	return true;
}

/* Waits until every entry up to seq has been answered; never from the worker. */
static void ovnfs_journal_wait_seq(struct ovnfs_sb *sbi, u64 seq)
{
	/* Acquire: pairs with ovnfs_batch_done, so the entries' effects are seen. */
	if (smp_load_acquire(&sbi->jdone) >= seq)
		return;
	if (WARN_ON_ONCE(current_work() == &sbi->jwork))
		return;
	ovnfs_journal_kick(sbi);
	/* Acquire: as above. */
	wait_event(sbi->jwait, smp_load_acquire(&sbi->jdone) >= seq);
}

/* Waits until the journal entries naming inode have been answered. */
void ovnfs_journal_wait_inode(struct inode *inode)
{
	ovnfs_journal_wait_seq(OVNFS_SB(inode->i_sb), READ_ONCE(OVNFS_I(inode)->jseq));
}

/* Waits until the journal entries naming any of nodes (NULLs skipped) have been answered. */
void ovnfs_journal_wait_nodes(struct inode *const *nodes, unsigned int n)
{
	struct ovnfs_sb *sbi = NULL;
	u64 seq = 0;
	unsigned int i;

	for (i = 0; i < n; i++) {
		if (!nodes[i])
			continue;
		sbi = OVNFS_SB(nodes[i]->i_sb);
		seq = max(seq, READ_ONCE(OVNFS_I(nodes[i])->jseq));
	}
	if (sbi)
		ovnfs_journal_wait_seq(sbi, seq);
}

/*
 * Says whether data of the file must not reach the daemon yet: its CREATE
 * or a size change is still in the journal, and data sent now would arrive
 * before it (or be cut by it).
 */
static bool ovnfs_journal_holds(struct inode *inode)
{
	u64 hold = READ_ONCE(OVNFS_I(inode)->hold_seq);

	/* Acquire: pairs with the worker's release of jdone. */
	return hold > smp_load_acquire(&OVNFS_SB(inode->i_sb)->jdone);
}

/*
 * Starts a writepages call for the file. Background writeback (wait unset)
 * leaves a file with unanswered journal entries to the journal and returns
 * false. Integrity writeback waits once for the entries naming the file when
 * it starts, so its data follows them, then runs even if newer entries
 * came meanwhile: waiting for those too could starve it (a file truncated
 * in a loop). While it runs (wb_active) the worker sends none of the file's
 * data or SETATTRs and no size change of it is appended, so neither newer
 * entry can overtake or cut its data on another port.
 */
bool ovnfs_writeback_begin(struct inode *inode, bool wait)
{
	struct ovnfs_sb *sbi = OVNFS_SB(inode->i_sb);
	struct ovnfs_inode *oi = OVNFS_I(inode);

	if (wait) {
		if (current_work() == &sbi->jwork)
			return false;
		ovnfs_journal_wait_inode(inode);
	}
	spin_lock(&sbi->jlock);
	if (!wait && ovnfs_pending(inode)) {
		spin_unlock(&sbi->jlock);
		return false;
	}
	oi->wb_active++;
	spin_unlock(&sbi->jlock);
	return true;
}

/* Ends a writepages call started by ovnfs_writeback_begin. */
void ovnfs_writeback_end(struct inode *inode)
{
	struct ovnfs_sb *sbi = OVNFS_SB(inode->i_sb);

	spin_lock(&sbi->jlock);
	OVNFS_I(inode)->wb_active--;
	spin_unlock(&sbi->jlock);
	wake_up_all(&sbi->jwait);
}

/* Waits until everything journaled so far has been answered. */
void ovnfs_journal_drain(struct ovnfs_sb *sbi)
{
	ovnfs_journal_wait_seq(sbi, READ_ONCE(sbi->jtail));
}

/*
 * Drains the journal and waits for the worker to finish: answered entries
 * drop their inode references only after jdone is published, and unmount
 * must not find them still held.
 */
void ovnfs_journal_quiesce(struct ovnfs_sb *sbi)
{
	ovnfs_journal_drain(sbi);
	flush_work(&sbi->jwork);
}

/*
 * Makes sure the daemon holds the file and everything this guest wrote to
 * it: its journal entries first, then the dirty pages. Returns the
 * write-back error, if any.
 */
int ovnfs_sync_inode(struct inode *inode)
{
	ovnfs_journal_wait_inode(inode);
	return filemap_write_and_wait(inode->i_mapping);
}

/* Says whether a file's data is pointless or impossible to send. */
static bool ovnfs_data_skip(struct inode *inode)
{
	const unsigned long *flags = &OVNFS_I(inode)->flags;

	return !inode->i_nlink || test_bit(OVNFS_I_ORPHAN, flags) || test_bit(OVNFS_I_BAD, flags);
}

/* Bytes of one file's data one entry copies into a batch. */
static u32 ovnfs_data_budget(struct ovnfs_sb *sbi)
{
	return min(OVNFS_SMALL, sbi->max_io);
}

/* Says whether a file is small enough to travel whole in one sub-request. */
static bool ovnfs_data_small(struct ovnfs_sb *sbi, struct inode *inode)
{
	return i_size_read(inode) <= ovnfs_data_budget(sbi);
}

/* Upper bound of the request bytes an entry adds to a batch. */
static u32 ovnfs_jent_req_max(const struct ovnfs_jent *e)
{
	switch (e->op) {
	case OVNFS_J_CREATE:
		return OVNFS_SUB_HDR + sizeof(struct ovnfs_create_req) + e->len + OVNFS_SMALL;
	case OVNFS_J_MKDIR:
		return OVNFS_SUB_HDR + sizeof(struct ovnfs_mkdir_req) + e->len;
	case OVNFS_J_UNLINK:
		return OVNFS_SUB_HDR + e->len;
	case OVNFS_J_RENAME:
		return OVNFS_SUB_HDR + sizeof(struct ovnfs_rename_req) + e->len + e->len2;
	case OVNFS_J_LINK:
		return OVNFS_SUB_HDR + sizeof(struct ovnfs_link_req) + e->len;
	case OVNFS_J_RMDIR:
		return OVNFS_SUB_HDR + e->len;
	case OVNFS_J_SETATTR:
		return OVNFS_SUB_HDR + sizeof(struct ovnfs_write_req) + OVNFS_SMALL +
		       OVNFS_SUB_HDR + sizeof(struct ovnfs_setattr_req);
	default:
		return OVNFS_DATA_REQ_MAX;
	}
}

/* Says whether the batch surely has room for entry e. */
static bool ovnfs_batch_room(const struct ovnfs_batch *b, const struct ovnfs_jent *e)
{
	u32 subs = e->op == OVNFS_J_DATA ? OVNFS_SMALL_PAGES + 1 : 2;

	return b->nents < OVNFS_BATCH_MAX_SUBS &&
	       b->nsubs + subs <= b->nsubs_cap &&
	       b->req_len + ovnfs_jent_req_max(e) <= b->req_cap &&
	       b->nfolios + OVNFS_SMALL_PAGES <= OVNFS_BATCH_MAX_FOLIOS;
}

/* Starts a sub-request at the end of the request buffer, reserving its header. */
static struct ovnfs_bsub *ovnfs_sub_start(struct ovnfs_batch *b, struct ovnfs_jent *e, u16 op)
{
	struct ovnfs_bsub *sub = &b->subs[b->nsubs++];

	sub->ent = e;
	sub->op = op;
	sub->hdr = b->req_len;
	sub->off = 0;
	sub->len = 0;
	sub->first = b->nfolios;
	sub->nr = 0;
	b->req_len += OVNFS_SUB_HDR;
	return sub;
}

/*
 * Writes a sub-request's length prefix and header; its body and data are
 * the bytes between the header and the end of the request buffer.
 */
static void ovnfs_sub_seal(struct ovnfs_batch *b, const struct ovnfs_bsub *sub, u64 id,
			   u32 uid, u32 gid)
{
	struct ovnfs_req req = {
		.op = cpu_to_le16(sub->op),
		.uid = cpu_to_le32(uid),
		.gid = cpu_to_le32(gid),
		.id = cpu_to_le64(id),
	};
	void *p = b->req + sub->hdr;

	put_unaligned_le32(b->req_len - sub->hdr - sizeof(__le32), p);
	memcpy(p + sizeof(__le32), &req, sizeof(req));
}

/* Appends bytes to the request buffer. */
static void ovnfs_put(struct ovnfs_batch *b, const void *p, u32 len)
{
	memcpy(b->req + b->req_len, p, len);
	b->req_len += len;
}

/* Called for every folio taken for write-back, with its bytes below EOF. */
typedef void (*ovnfs_folio_fn)(struct ovnfs_batch *b, void *ctx, struct folio *folio,
			       loff_t pos, u32 len);

/*
 * Takes up to max_pages dirty folios of inode below end for write-back:
 * each is cleaned, put under writeback, handed to fn while locked (the size
 * is read under the folio lock, so a racing extension is either seen or
 * redirties the folio) and unlocked. Folios wholly beyond EOF are cleaned
 * without data. Folios already under writeback are skipped, never waited
 * for (they stay dirty). Returns the number of folios handed to fn.
 */
static u32 ovnfs_take_folios(struct ovnfs_batch *b, struct inode *inode, loff_t end,
			     u32 max_pages, ovnfs_folio_fn fn, void *ctx)
{
	struct writeback_control wbc = {
		.sync_mode = WB_SYNC_NONE,
		.nr_to_write = max_pages,
		.range_start = 0,
		.range_end = end - 1,
	};
	struct folio *folio = NULL;
	u32 taken = 0;
	int error = 0;

	set_bit(OVNFS_I_IN_BATCH, &OVNFS_I(inode)->flags);
	wbc_attach_fdatawrite_inode(&wbc, inode);
	while ((folio = writeback_iter(inode->i_mapping, &wbc, folio, &error))) {
		loff_t pos = folio_pos(folio);
		loff_t size = i_size_read(inode);

		folio_start_writeback(folio);
		if (pos >= size) {
			folio_end_writeback(folio);
			folio_unlock(folio);
			continue;
		}
		fn(b, ctx, folio, pos, min_t(loff_t, folio_size(folio), size - pos));
		b->folios[b->nfolios++] = folio;
		folio_unlock(folio);
		taken++;
	}
	wbc_detach_inode(&wbc);
	return taken;
}

/* Ends writeback of a run of the batch's folios, redirtying them first if asked. */
static void ovnfs_end_folios(struct ovnfs_batch *b, u32 first, u32 nr,
			     enum ovnfs_sub_outcome outcome)
{
	u32 i;

	for (i = first; i < first + nr; i++) {
		struct folio *folio = b->folios[i];

		/* Writeback keeps truncation off the folio while it is redirtied. */
		if (outcome != OVNFS_SUB_DONE)
			folio_mark_dirty(folio);
		/*
		 * Sent: before it can leave the cache, reads of it must go to
		 * the daemon. Not sent: it stays dirty, and the zero point must
		 * not rise while a truncation it would follow may be pending.
		 */
		if (outcome == OVNFS_SUB_DONE)
			ovnfs_zero_past(folio->mapping->host,
					folio_pos(folio) + folio_size(folio));
		folio_end_writeback(folio);
	}
}

/* Where a WRITE run being built stands. */
struct ovnfs_run {
	struct ovnfs_jent *ent;
	struct ovnfs_bsub *sub;
};

/* Seals the WRITE run being built. */
static void ovnfs_run_seal(struct ovnfs_batch *b, struct ovnfs_run *r)
{
	struct ovnfs_write_req wr;
	struct ovnfs_bsub *sub = r->sub;

	if (!sub)
		return;
	wr.off = cpu_to_le64(sub->off);
	wr.len = cpu_to_le32(sub->len);
	wr.pad = 0;
	memcpy(b->req + sub->hdr + OVNFS_SUB_HDR, &wr, sizeof(wr));
	ovnfs_sub_seal(b, sub, OVNFS_I(r->ent->inode)->id, r->ent->uid, r->ent->gid);
	r->sub = NULL;
}

/* ovnfs_take_folios sink for DATA: contiguous folios form one WRITE. */
static void ovnfs_run_add(struct ovnfs_batch *b, void *ctx, struct folio *folio, loff_t pos, u32 len)
{
	struct ovnfs_run *r = ctx;

	if (r->sub && r->sub->off + r->sub->len != pos)
		ovnfs_run_seal(b, r);
	if (!r->sub) {
		r->sub = ovnfs_sub_start(b, r->ent, OVNFS_OP_WRITE);
		r->sub->off = pos;
		r->sub->first = b->nfolios;
		b->req_len += sizeof(struct ovnfs_write_req);
	}
	memcpy_from_folio(b->req + b->req_len, folio, 0, len);
	b->req_len += len;
	r->sub->len += len;
	r->sub->nr++;
}

/* Appends the SETATTR that gives the daemon a file's local mtime. */
static void ovnfs_pack_mtime(struct ovnfs_batch *b, struct ovnfs_jent *e)
{
	struct ovnfs_bsub *sub = ovnfs_sub_start(b, e, OVNFS_OP_SETATTR);
	struct ovnfs_setattr_req sa;

	e->sent_mtime = ovnfs_mtime_req(e->inode, &sa);
	ovnfs_put(b, &sa, sizeof(sa));
	ovnfs_sub_seal(b, sub, OVNFS_I(e->inode)->id, e->uid, e->gid);
}

/* Takes back what was packed since a mark: the folios are redirtied. */
static void ovnfs_unpack(struct ovnfs_batch *b, u32 nsubs, u32 req_len, u32 nfolios)
{
	ovnfs_end_folios(b, nfolios, b->nfolios - nfolios, OVNFS_SUB_UNKNOWN);
	b->nfolios = nfolios;
	b->nsubs = nsubs;
	b->req_len = req_len;
}

/*
 * Packs up to a budget of a file's dirty data: WRITE runs, then its mtime.
 * What the budget leaves is queued again behind, so a large file streams
 * through successive batches. A size change journaled meanwhile (after this
 * entry) would cut what was taken: then the data is given back and queued
 * again behind it.
 */
static void ovnfs_pack_data(struct ovnfs_sb *sbi, struct ovnfs_batch *b, struct ovnfs_jent *e)
{
	struct inode *inode = e->inode;
	struct ovnfs_run r = { .ent = e };
	u32 subs = b->nsubs, req_len = b->req_len, nfolios = b->nfolios;
	u32 budget = ovnfs_data_budget(sbi) >> PAGE_SHIFT;

	e->requeue = ovnfs_take_folios(b, inode, LLONG_MAX, budget, ovnfs_run_add, &r) == budget;
	ovnfs_run_seal(b, &r);
	if (READ_ONCE(OVNFS_I(inode)->hold_seq) > e->seq) {
		ovnfs_unpack(b, subs, req_len, nfolios);
		e->requeue = 1;
		return;
	}
	if (b->nsubs != subs)
		ovnfs_pack_mtime(b, e);
}

/* Where a CREATE's content is being copied to. */
struct ovnfs_content {
	void *base;
};

/* ovnfs_take_folios sink for CREATE content: copies a folio to its offset. */
static void ovnfs_content_add(struct ovnfs_batch *b, void *ctx, struct folio *folio,
			      loff_t pos, u32 len)
{
	struct ovnfs_content *c = ctx;

	memcpy_from_folio(c->base + pos, folio, 0, len);
}

/*
 * Copies a closed small file's content behind its CREATE. Pages not in the
 * cache are holes (the daemon has no data before the CREATE) and stay zero.
 * Returns the content length, 0 when the file cannot go this way; on
 * success the file's DATA entry is consumed unless pages stayed dirty.
 */
static u32 ovnfs_pack_content(struct ovnfs_sb *sbi, struct ovnfs_batch *b, struct ovnfs_jent *e)
{
	struct inode *inode = e->inode;
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_content c = { .base = b->req + b->req_len };
	struct ovnfs_jent *data;
	loff_t size = i_size_read(inode);

	if (!size || !ovnfs_data_small(sbi, inode) || atomic_read(&oi->opens) ||
	    mapping_writably_mapped(inode->i_mapping) || ovnfs_data_skip(inode) ||
	    READ_ONCE(oi->hold_seq) != e->seq)
		return 0;
	memset(c.base, 0, size);
	e->first = b->nfolios;
	ovnfs_take_folios(b, inode, round_up(size, PAGE_SIZE), OVNFS_SMALL_PAGES, ovnfs_content_add,
			  &c);
	e->nr = b->nfolios - e->first;
	if (i_size_read(inode) != size || READ_ONCE(oi->hold_seq) != e->seq) {
		/* Changed meanwhile: give the folios back and send the CREATE bare. */
		ovnfs_end_folios(b, e->first, e->nr, OVNFS_SUB_UNKNOWN);
		b->nfolios = e->first;
		e->nr = 0;
		return 0;
	}
	e->content = 1;
	ovnfs_count(OVNFS_C_CREATE_DATA, 1);
	if (mapping_tagged(inode->i_mapping, PAGECACHE_TAG_DIRTY))
		return size;
	spin_lock(&sbi->jlock);
	data = oi->data_ent;
	if (data) {
		list_del_init(&data->node);
		oi->data_ent = NULL;
	}
	spin_unlock(&sbi->jlock);
	if (data)
		ovnfs_jent_free(sbi, data);
	return size;
}

/* Packs a CREATE, with the file's content when it qualifies. */
static void ovnfs_pack_create(struct ovnfs_sb *sbi, struct ovnfs_batch *b, struct ovnfs_jent *e)
{
	struct ovnfs_bsub *sub = ovnfs_sub_start(b, e, OVNFS_OP_CREATE);
	struct ovnfs_create_req cr = {
		.mode = cpu_to_le32(e->mode),
		.flags = cpu_to_le32(OVNFS_CREATE_EXCL),
		.new_id = cpu_to_le64(OVNFS_I(e->inode)->id),
		.name_len = cpu_to_le16(e->len),
	};
	u32 at = b->req_len, data_len;
	struct timespec64 ts;

	b->req_len += sizeof(cr) + e->len;
	data_len = ovnfs_pack_content(sbi, b, e);
	b->req_len += data_len;
	spin_lock(&e->inode->i_lock);
	ts = inode_get_mtime(e->inode);
	spin_unlock(&e->inode->i_lock);
	e->sent_mtime = timespec64_to_ns(&ts);
	cr.mtime_ns = cpu_to_le64(e->sent_mtime);
	cr.data_len = cpu_to_le32(data_len);
	memcpy(b->req + at, &cr, sizeof(cr));
	memcpy(b->req + at + sizeof(cr), e->names, e->len);
	ovnfs_sub_seal(b, sub, OVNFS_I(e->dir)->id, e->uid, e->gid);
}

/* Takes a file's queued DATA entry out of the journal, its data being sent otherwise. */
static void ovnfs_consume_data(struct ovnfs_sb *sbi, struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_jent *data;

	spin_lock(&sbi->jlock);
	data = oi->data_ent;
	if (data) {
		list_del_init(&data->node);
		oi->data_ent = NULL;
	}
	spin_unlock(&sbi->jlock);
	if (data)
		ovnfs_jent_free(sbi, data);
}

/*
 * Says whether a size SETATTR may carry the file's whole content: the file
 * is closed, small, and its queued DATA entry is the only later entry
 * naming it (a truncated and rewritten file, typically).
 */
static bool ovnfs_setattr_mergeable(struct ovnfs_sb *sbi, struct ovnfs_jent *e)
{
	struct inode *inode = e->inode;
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_jent *data;
	bool ok;

	if (!(e->valid & OVNFS_SET_SIZE) || !i_size_read(inode) || !ovnfs_data_small(sbi, inode) ||
	    atomic_read(&oi->opens) || mapping_writably_mapped(inode->i_mapping) ||
	    ovnfs_data_skip(inode))
		return false;
	spin_lock(&sbi->jlock);
	data = oi->data_ent;
	ok = data && READ_ONCE(oi->jseq) == data->seq;
	spin_unlock(&sbi->jlock);
	return ok;
}

/*
 * Packs the whole content of a truncated and rewritten file as one WRITE
 * from offset 0, so the SETATTR that follows can set the final size and
 * mtime instead of truncating first: valid only when every byte below the
 * size is dirty and taken. Returns false, with nothing packed, otherwise.
 */
static bool ovnfs_pack_merged(struct ovnfs_sb *sbi, struct ovnfs_batch *b, struct ovnfs_jent *e)
{
	struct inode *inode = e->inode;
	struct ovnfs_run r = { .ent = e };
	u32 subs = b->nsubs, req_len = b->req_len, nfolios = b->nfolios;
	loff_t size = i_size_read(inode);
	struct ovnfs_bsub *sub;

	if (size > ovnfs_data_budget(sbi))
		return false;
	ovnfs_take_folios(b, inode, round_up(size, PAGE_SIZE), OVNFS_SMALL_PAGES, ovnfs_run_add, &r);
	sub = r.sub;
	ovnfs_run_seal(b, &r);
	if (b->nsubs != subs + 1 || sub->off || sub->len != size || i_size_read(inode) != size ||
	    mapping_tagged(inode->i_mapping, PAGECACHE_TAG_DIRTY) ||
	    READ_ONCE(OVNFS_I(inode)->hold_seq) != e->seq) {
		ovnfs_unpack(b, subs, req_len, nfolios);
		return false;
	}
	ovnfs_consume_data(sbi, inode);
	e->merged = 1;
	e->size = size;
	return true;
}

/* Packs a SETATTR, merged with the file's content when that is sound. */
static void ovnfs_pack_setattr(struct ovnfs_sb *sbi, struct ovnfs_batch *b, struct ovnfs_jent *e)
{
	struct ovnfs_setattr_req sa = {
		.mode = cpu_to_le32(e->mode),
	};
	struct ovnfs_bsub *sub;
	u32 valid = e->valid;
	struct timespec64 ts;
	s64 mtime;

	if (ovnfs_setattr_mergeable(sbi, e) && ovnfs_pack_merged(sbi, b, e))
		valid |= OVNFS_SET_MTIME;
	/* Rule 5: the mtime the guest shows now, not the one of the syscall. */
	spin_lock(&e->inode->i_lock);
	ts = inode_get_mtime(e->inode);
	spin_unlock(&e->inode->i_lock);
	mtime = timespec64_to_ns(&ts);
	e->sent_mtime = mtime;
	sa.valid = cpu_to_le32(valid);
	sa.size = cpu_to_le64(e->size);
	sa.mtime_ns = cpu_to_le64(mtime);
	sub = ovnfs_sub_start(b, e, OVNFS_OP_SETATTR);
	ovnfs_put(b, &sa, sizeof(sa));
	ovnfs_sub_seal(b, sub, OVNFS_I(e->inode)->id, e->uid, e->gid);
}

/* Packs one namespace entry as its sub-request. */
static void ovnfs_pack_namespace(struct ovnfs_sb *sbi, struct ovnfs_batch *b, struct ovnfs_jent *e)
{
	struct ovnfs_bsub *sub;
	u64 id;

	if (e->op == OVNFS_J_SETATTR) {
		ovnfs_pack_setattr(sbi, b, e);
		return;
	}
	id = OVNFS_I(e->dir)->id;
	switch (e->op) {
	case OVNFS_J_CREATE:
		ovnfs_pack_create(sbi, b, e);
		return;
	case OVNFS_J_RMDIR:
		sub = ovnfs_sub_start(b, e, OVNFS_OP_RMDIR);
		break;
	case OVNFS_J_MKDIR: {
		struct ovnfs_mkdir_req mr = {
			.mode = cpu_to_le32(e->mode),
			.new_id = cpu_to_le64(OVNFS_I(e->inode)->id),
			.mtime_ns = cpu_to_le64(e->mtime),
		};

		sub = ovnfs_sub_start(b, e, OVNFS_OP_MKDIR);
		ovnfs_put(b, &mr, sizeof(mr));
		break;
	}
	case OVNFS_J_UNLINK:
		sub = ovnfs_sub_start(b, e, OVNFS_OP_UNLINK);
		break;
	case OVNFS_J_RENAME: {
		struct ovnfs_rename_req rr = {
			.new_dir = cpu_to_le64(OVNFS_I(e->dir2)->id),
			.flags = cpu_to_le32(e->flags),
			.old_len = cpu_to_le16(e->len),
			.new_len = cpu_to_le16(e->len2),
		};

		sub = ovnfs_sub_start(b, e, OVNFS_OP_RENAME);
		ovnfs_put(b, &rr, sizeof(rr));
		break;
	}
	default: {
		struct ovnfs_link_req lr = { .dir = cpu_to_le64(id) };

		sub = ovnfs_sub_start(b, e, OVNFS_OP_LINK);
		ovnfs_put(b, &lr, sizeof(lr));
		id = OVNFS_I(e->inode)->id;
		break;
	}
	}
	ovnfs_put(b, e->names, e->len + e->len2);
	ovnfs_sub_seal(b, sub, id, e->uid, e->gid);
}

/* Says whether an entry may carry the data of its file (and lock its folios). */
static bool ovnfs_jent_has_data(const struct ovnfs_jent *e)
{
	return e->op == OVNFS_J_DATA || e->op == OVNFS_J_CREATE ||
	       (e->op == OVNFS_J_SETATTR && (e->valid & OVNFS_SET_SIZE));
}

/*
 * Takes the oldest entry into the batch. Returns false when there is none,
 * or when it might not fit or carries data of a file already in the batch.
 */
static bool ovnfs_batch_take(struct ovnfs_sb *sbi, struct ovnfs_batch *b)
{
	struct ovnfs_jent *e;

	spin_lock(&sbi->jlock);
	e = list_first_entry_or_null(&sbi->jlist, struct ovnfs_jent, node);
	/* Acquire: pairs with ovnfs_journal_release. */
	if (!e || !smp_load_acquire(&e->ready) || !ovnfs_batch_room(b, e) ||
	    (ovnfs_jent_has_data(e) && test_bit(OVNFS_I_IN_BATCH, &OVNFS_I(e->inode)->flags))) {
		spin_unlock(&sbi->jlock);
		return false;
	}
	/*
	 * A writepages call sending the file's data and mtime runs on another
	 * port: the file's data and SETATTRs wait for it, alone in a batch.
	 */
	if ((e->op == OVNFS_J_DATA || e->op == OVNFS_J_SETATTR) &&
	    OVNFS_I(e->inode)->wb_active) {
		struct inode *inode = e->inode;

		spin_unlock(&sbi->jlock);
		if (b->nents)
			return false;
		wait_event(sbi->jwait, !ovnfs_writeback_active(sbi, inode));
		return true;
	}
	list_del_init(&e->node);
	if (e->op == OVNFS_J_DATA)
		OVNFS_I(e->inode)->data_ent = NULL;
	spin_unlock(&sbi->jlock);

	b->ents[b->nents++] = e;
	e->outcome = OVNFS_SUB_DONE;
	if (e->op != OVNFS_J_DATA)
		ovnfs_pack_namespace(sbi, b, e);
	else if (!ovnfs_data_skip(e->inode))
		ovnfs_pack_data(sbi, b, e);
	return true;
}

/* Logs and records a journal entry the daemon refused or did not answer. */
static void ovnfs_jent_report(struct super_block *sb, const struct ovnfs_jent *e, int err)
{
	struct inode *dir = e->dir ? e->dir : e->inode;

	ovnfs_count(OVNFS_C_LATE_FAILURES, 1);
	pr_warn_ratelimited("ovnfs: deferred %s of %llu/%.*s failed: errno %d\n",
			    ovnfs_jop_names[e->op], OVNFS_I(dir)->id, e->len, e->names, -err);
	errseq_set(&sb->s_wb_err, err);
}

/*
 * Records a failure on the mapping of a node involved, for the next fsync
 * (and, for a regular file, close) to report: a directory the operation
 * changed names in, a file it changed, a directory whose attributes it set.
 */
static void ovnfs_jent_node_error(struct inode *inode, int err)
{
	if (inode && (S_ISREG(inode->i_mode) || S_ISDIR(inode->i_mode)))
		mapping_set_error(inode->i_mapping, err);
}

/* Drops a directory's complete listing; safe without dir_lock (see dir.c). */
static void ovnfs_dir_doubt_lockless(struct inode *dir)
{
	if (dir)
		WRITE_ONCE(OVNFS_I(dir)->dir_gen, 0);
}

/*
 * Makes the guest's view of a namespace entry converge on the daemon's after
 * the entry failed or went unanswered: listings, names and attributes are
 * revalidated on next use. A refused CREATE/MKDIR leaves a bad node.
 */
static void ovnfs_jent_failed(struct super_block *sb, struct ovnfs_jent *e, int err)
{
	struct inode *inode = e->inode;
	bool refused = err != -EAGAIN;

	ovnfs_jent_report(sb, e, refused ? err : -EIO);
	ovnfs_dir_doubt_lockless(e->dir);
	ovnfs_dir_doubt_lockless(e->dir2);
	if ((e->op == OVNFS_J_CREATE || e->op == OVNFS_J_MKDIR) && refused)
		set_bit(OVNFS_I_BAD, &OVNFS_I(inode)->flags);
	ovnfs_mark_stale(inode);
	if (e->target)
		ovnfs_mark_stale(e->target);
	ovnfs_forget_name(e->dir, e->names, e->len);
	if (e->op == OVNFS_J_RENAME)
		ovnfs_forget_name(e->dir2, e->names + e->len, e->len2);
	if (!refused)
		err = -EIO;
	ovnfs_jent_node_error(e->dir, err);
	ovnfs_jent_node_error(e->dir2, err);
	if (S_ISREG(inode->i_mode) || e->op == OVNFS_J_SETATTR)
		ovnfs_jent_node_error(inode, err);
	if (e->target && S_ISREG(e->target->i_mode))
		ovnfs_jent_node_error(e->target, err);
}

/*
 * Applies the reply to a namespace entry. Attributes in it are applied only
 * when no later entry names the inode, and never move its times. err is
 * -EAGAIN when no reply arrived.
 */
static void ovnfs_finish_namespace(struct super_block *sb, struct ovnfs_batch *b,
				   struct ovnfs_jent *e, int err, const struct ovnfs_attr *a, u64 gen,
				   u64 since)
{
	struct inode *inode = e->inode;
	struct ovnfs_inode *oi = OVNFS_I(inode);
	bool has_attr = e->op == OVNFS_J_CREATE || e->op == OVNFS_J_MKDIR ||
			e->op == OVNFS_J_LINK || e->op == OVNFS_J_SETATTR;

	if (err == -EMSGSIZE)
		err = -EAGAIN;
	if (!err && has_attr && le64_to_cpu(a->id) != oi->id)
		err = -EIO;
	if (err) {
		if (e->content)
			ovnfs_end_folios(b, e->first, e->nr,
					 err == -EAGAIN ? OVNFS_SUB_UNKNOWN : OVNFS_SUB_DONE);
		ovnfs_jent_failed(sb, e, err);
		return;
	}
	/* Applied while the content's writeback keeps the local size authoritative. */
	if (has_attr && READ_ONCE(oi->jseq) == e->seq)
		ovnfs_apply_attr(inode, a, gen, OVNFS_ATTR_DATA, since);
	else if (has_attr)
		ovnfs_note_server(inode, a, since);
	if (e->content) {
		ovnfs_end_folios(b, e->first, e->nr, OVNFS_SUB_DONE);
		ovnfs_mtime_synced(inode, a, e->sent_mtime);
	}
	if (e->merged)
		ovnfs_mtime_synced(inode, a, e->sent_mtime);
}

/* Applies the reply to one of a DATA entry's WRITEs and ends its folios' writeback. */
static void ovnfs_finish_write(struct ovnfs_batch *b, const struct ovnfs_bsub *sub, int err,
			       const struct ovnfs_attr *a, u64 gen, u64 since)
{
	struct ovnfs_jent *e = sub->ent;
	struct inode *inode = e->inode;
	enum ovnfs_sub_outcome outcome = OVNFS_SUB_DONE;

	/* A file that went away while its data was in flight drops the error. */
	if (err == -EMSGSIZE)
		outcome = OVNFS_SUB_ALONE;
	else if (err == -EAGAIN)
		outcome = OVNFS_SUB_UNKNOWN;
	else if (err && !ovnfs_data_skip(inode))
		ovnfs_writeback_error(inode, err);
	else if (!err && le64_to_cpu(a->id) == OVNFS_I(inode)->id &&
		 READ_ONCE(OVNFS_I(inode)->jseq) == e->seq)
		ovnfs_apply_attr(inode, a, gen, OVNFS_ATTR_DATA, since);
	else if (!err && le64_to_cpu(a->id) == OVNFS_I(inode)->id)
		ovnfs_note_server(inode, a, since);
	ovnfs_end_folios(b, sub->first, sub->nr, outcome);
	e->outcome = max(e->outcome, outcome);
}

/* Applies the reply to a DATA entry's mtime SETATTR. */
static void ovnfs_finish_mtime(struct ovnfs_jent *e, int err, const struct ovnfs_attr *a, u64 gen,
			       u64 since)
{
	struct inode *inode = e->inode;

	if (err == -EMSGSIZE || err == -EAGAIN) {
		e->mtime_unsent = 1;
	} else if (err && ovnfs_data_skip(inode)) {
		/* The file went away while its data was in flight. */
	} else if (err) {
		ovnfs_writeback_error(inode, err);
	} else if (le64_to_cpu(a->id) == OVNFS_I(inode)->id) {
		if (READ_ONCE(OVNFS_I(inode)->jseq) == e->seq)
			ovnfs_apply_attr(inode, a, gen, OVNFS_ATTR_DATA, since);
		else
			ovnfs_note_server(inode, a, since);
		ovnfs_mtime_synced(inode, a, e->sent_mtime);
	}
}

/*
 * Sends the batch as one COMPOUND and applies the sub-replies in order,
 * feeding each one's epoch to the cache layer. Sub-requests without a
 * well-formed reply count as unanswered (-EAGAIN).
 */
static void ovnfs_batch_send(struct ovnfs_sb *sbi, struct ovnfs_batch *b)
{
	struct ovnfs_msg m = {
		.op = OVNFS_OP_COMPOUND,
		.id = OVNFS_ROOT_ID,
		.obuf = b->req,
		.obuf_len = b->req_len,
		.buf = b->rep,
		.buf_len = b->nsubs * OVNFS_SUB_REP_MAX,
	};
	u32 got, off = 0, i;
	u64 since;

	if (!b->nsubs)
		return;
	ovnfs_count(OVNFS_C_BATCHED, b->nsubs);
	/* Replies are not applied over changes this guest makes meanwhile. */
	since = atomic64_read(&sbi->attr_seq);
	got = ovnfs_call(sbi, &m) ? 0 : m.data_got;
	for (i = 0; i < b->nsubs; i++) {
		const struct ovnfs_bsub *sub = &b->subs[i];
		struct ovnfs_attr a = {};
		struct ovnfs_rep rep;
		int err = -EAGAIN;
		u64 gen = 0;
		u32 len = 0;

		if (got - off >= sizeof(__le32)) {
			len = get_unaligned_le32(b->rep + off);
			if (len < sizeof(rep) || len > got - off - sizeof(__le32))
				len = 0;
		}
		if (len) {
			memcpy(&rep, b->rep + off + sizeof(__le32), sizeof(rep));
			gen = ovnfs_note_epoch(sbi, le64_to_cpu(rep.epoch));
			err = rep.error ? ovnfs_errno(le32_to_cpu(rep.error)) : 0;
			if (len >= sizeof(rep) + sizeof(a))
				memcpy(&a, b->rep + off + sizeof(__le32) + sizeof(rep), sizeof(a));
			off += sizeof(__le32) + len;
		} else {
			off = got;
		}
		switch (sub->op) {
		case OVNFS_OP_WRITE:
			ovnfs_finish_write(b, sub, err, &a, gen, since);
			break;
		case OVNFS_OP_SETATTR:
			if (sub->ent->op == OVNFS_J_DATA)
				ovnfs_finish_mtime(sub->ent, err, &a, gen, since);
			else
				ovnfs_finish_namespace(sbi->sb, b, sub->ent, err, &a, gen, since);
			break;
		default:
			ovnfs_finish_namespace(sbi->sb, b, sub->ent, err, &a, gen, since);
			break;
		}
	}
}

/*
 * Lets go of a DATA entry's file: an mtime SETATTR that did not run is sent
 * alone, and dirty data is queued again when it was refused for size, the
 * budget left it or a later size change held it back.
 */
static void ovnfs_data_release(struct ovnfs_jent *e)
{
	struct inode *inode = e->inode;

	if (e->outcome == OVNFS_SUB_DONE && e->mtime_unsent)
		ovnfs_send_mtime(inode);
	if (!mapping_tagged(inode->i_mapping, PAGECACHE_TAG_DIRTY))
		return;
	if (e->outcome == OVNFS_SUB_ALONE || (e->outcome == OVNFS_SUB_DONE && e->requeue) ||
	    ovnfs_journal_holds(inode))
		ovnfs_journal_data(inode);
}

/*
 * Publishes how far the journal got: everything before the oldest entry not
 * yet taken, or everything when none is left (entries cancelled or consumed
 * while the worker was idle count too). Called by the worker between
 * batches only, so no taken entry is still unanswered.
 */
static void ovnfs_journal_publish(struct ovnfs_sb *sbi)
{
	struct ovnfs_jent *head;

	spin_lock(&sbi->jlock);
	head = list_first_entry_or_null(&sbi->jlist, struct ovnfs_jent, node);
	/* Release: waiters that see the new jdone see what the entries did. */
	smp_store_release(&sbi->jdone, head ? head->seq - 1 : sbi->jtail);
	spin_unlock(&sbi->jlock);
	wake_up_all(&sbi->jwait);
}

/*
 * Finishes the batch's entries, publishes how far the journal got, and only
 * then drops their references: the last reference to an unlinked inode
 * evicts it, and eviction waits for writeback of the inode that may be
 * waiting for exactly this publication (integrity writeback of a file with
 * unanswered entries).
 */
static void ovnfs_batch_done(struct ovnfs_sb *sbi, struct ovnfs_batch *b)
{
	u32 i;

	for (i = 0; i < b->nents; i++) {
		struct ovnfs_jent *e = b->ents[i];

		if (ovnfs_jent_has_data(e))
			clear_bit(OVNFS_I_IN_BATCH, &OVNFS_I(e->inode)->flags);
		if (e->op == OVNFS_J_DATA)
			ovnfs_data_release(e);
		else if (e->merged && e->outcome != OVNFS_SUB_DONE)
			ovnfs_journal_data(e->inode);
	}
	ovnfs_journal_publish(sbi);
	for (i = 0; i < b->nents; i++)
		ovnfs_jent_free(sbi, b->ents[i]);
	wake_up_all(&sbi->jwait);
	b->req_len = 0;
	b->nsubs = 0;
	b->nents = 0;
	b->nfolios = 0;
}

/* The worker: builds, sends and finishes batches until the journal is empty. */
static void ovnfs_journal_work(struct work_struct *work)
{
	struct ovnfs_sb *sbi = container_of(work, struct ovnfs_sb, jwork);
	struct ovnfs_batch *b = sbi->batch;

	for (;;) {
		while (ovnfs_batch_take(sbi, b))
			;
		if (!b->nents) {
			ovnfs_journal_publish(sbi);
			return;
		}
		ovnfs_batch_send(sbi, b);
		ovnfs_batch_done(sbi, b);
	}
}

/* Asks the daemon for a run of ids; any refusal turns the pool off for good. */
void ovnfs_ids_fill(struct ovnfs_sb *sbi)
{
	struct ovnfs_alloc_ids_req req = { .count = cpu_to_le32(OVNFS_ID_BATCH) };
	struct ovnfs_alloc_ids_rep rep;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_ALLOC_IDS,
		.id = OVNFS_ROOT_ID,
		.arg = &req,
		.arg_len = sizeof(req),
		.res = &rep,
		.res_len = sizeof(rep),
	};
	u64 first, count;
	int err;

	err = ovnfs_call(sbi, &m);
	first = le64_to_cpu(rep.first);
	count = le32_to_cpu(rep.count);
	if (!err && (!count || count > OVNFS_ID_BATCH || first <= OVNFS_ROOT_ID ||
		     first + count < first))
		err = -EIO;
	spin_lock(&sbi->jlock);
	if (!err) {
		if (sbi->id_next == sbi->id_end) {
			sbi->id_next = first;
			sbi->id_end = first + count;
		} else {
			sbi->spare_next = first;
			sbi->spare_end = first + count;
		}
	} else if (!m.status) {
		sbi->ids_off = 1;
	}
	sbi->id_refilling = 0;
	spin_unlock(&sbi->jlock);
	if (!err)
		ovnfs_count(OVNFS_C_ID_REFILLS, 1);
	else if (!m.status)
		pr_info("ovnfs: no ids for creates ahead of the daemon (errno %d): creates are synchronous\n",
			-err);
}

/* Refills the id pool in the background. */
static void ovnfs_ids_work(struct work_struct *work)
{
	ovnfs_ids_fill(container_of(work, struct ovnfs_sb, id_work));
}

/*
 * Takes an id for a node created ahead of the daemon, or returns 0 when the
 * pool is empty (the caller creates synchronously). Starts a refill when the
 * stock runs low, so callers never wait for one.
 */
u64 ovnfs_id_take(struct ovnfs_sb *sbi)
{
	bool refill = false;
	u64 id = 0;

	spin_lock(&sbi->jlock);
	if (sbi->id_next == sbi->id_end && sbi->spare_next != sbi->spare_end) {
		sbi->id_next = sbi->spare_next;
		sbi->id_end = sbi->spare_end;
		sbi->spare_next = 0;
		sbi->spare_end = 0;
	}
	if (sbi->id_next != sbi->id_end)
		id = sbi->id_next++;
	if (!sbi->ids_off && !sbi->id_refilling && sbi->spare_next == sbi->spare_end &&
	    sbi->id_end - sbi->id_next < OVNFS_ID_LOW) {
		sbi->id_refilling = 1;
		refill = true;
	}
	spin_unlock(&sbi->jlock);
	if (refill)
		queue_work(system_unbound_wq, &sbi->id_work);
	return id;
}

/* Sets up the journal and allocates the worker's batch. */
int ovnfs_journal_init(struct ovnfs_sb *sbi)
{
	spin_lock_init(&sbi->jlock);
	INIT_LIST_HEAD(&sbi->jlist);
	init_waitqueue_head(&sbi->jwait);
	INIT_WORK(&sbi->jwork, ovnfs_journal_work);
	INIT_WORK(&sbi->id_work, ovnfs_ids_work);
	sbi->batch = ovnfs_batch_alloc();
	return sbi->batch ? 0 : -ENOMEM;
}

/* Stops the workers (the journal is empty after the final sync) and frees the batch. */
void ovnfs_journal_exit(struct ovnfs_sb *sbi)
{
	if (!sbi->batch)
		return;
	cancel_work_sync(&sbi->jwork);
	cancel_work_sync(&sbi->id_work);
	WARN_ON_ONCE(!list_empty(&sbi->jlist));
	ovnfs_batch_free(sbi->batch);
	sbi->batch = NULL;
}
