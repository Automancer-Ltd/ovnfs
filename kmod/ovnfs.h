/* SPDX-License-Identifier: GPL-2.0-only */
/*
 * ovnfs.h - internal declarations of the ovnfs guest filesystem.
 *
 * Cache model: every reply carries the tree's epoch. The superblock turns
 * epochs into a local generation (gen) that is bumped whenever a reply shows
 * an epoch different from the last one seen. Dentries (d_time), inode
 * attributes (attr_gen), directory listings (dir_gen) and symlink targets
 * (link_gen) record the generation they were validated under and stay valid
 * while it equals the superblock's.
 */
#ifndef OVNFS_H
#define OVNFS_H

#include <linux/atomic.h>
#include <linux/fs.h>
#include <linux/limits.h>
#include <linux/mutex.h>
#include <linux/rbtree.h>
#include <linux/spinlock.h>
#include <linux/wait.h>
#include <linux/workqueue.h>

#include "ovnfs_wire.h"

struct ovnfs_batch;
struct ovnfs_jent;

#define OVNFS_SUPER_MAGIC	0x53464e56

/*
 * Largest request the transport builds: header, the largest body, then two
 * names or a name and a symlink target (at most PATH_MAX - 1 bytes).
 */
#define OVNFS_REQ_MAX	(sizeof(struct ovnfs_req) + sizeof(struct ovnfs_setattr_req) + \
			 OVNFS_NAME_MAX + PATH_MAX)
/* Largest fixed result behind struct ovnfs_rep that a call copies out. */
#define OVNFS_RES_MAX	128
/* Most page-cache folios one READ or WRITE carries. */
#define OVNFS_MAX_FOLIOS	(OVNFS_MAX_IO / PAGE_SIZE)

/*
 * One call. The request is struct ovnfs_req, then arg, name and name2. The
 * reply's fixed result is copied to res; bulk data moves between the daemon
 * and either page-cache folios (WRITE sends them, READ fills them) or a
 * kmalloc/vmalloc buffer (READDIR, READLINK fill it), never through a copy.
 * obuf carries a large request body by address (COMPOUND).
 */
struct ovnfs_msg {
	u16 op;
	u64 id;
	const void *arg;
	u32 arg_len;
	const void *name;
	u32 name_len;
	const void *name2;
	u32 name2_len;
	void *res;
	u32 res_len;
	struct folio **folios;
	unsigned int nr_folios;
	u32 data_len;		/* folio bytes to send or receive */
	void *buf;
	u32 buf_len;
	const void *obuf;	/* request bytes sent by address after the names */
	u32 obuf_len;
	/* out */
	u32 status;		/* transport status: nonzero when no reply arrived */
	u32 data_got;		/* bulk bytes received */
	u64 gen;		/* generation of the reply's epoch */
};

/* Per-superblock state. */
struct ovnfs_sb {
	struct super_block *sb;
	spinlock_t epoch_lock;	/* serializes epoch/gen updates */
	u64 epoch;		/* last epoch seen */
	u64 gen;		/* local generation, bumped when the epoch changes */
	atomic64_t attr_seq;	/* bumped by every attribute change this guest made */
	atomic64_t evict_seq;	/* bumped when an inode this guest changed is evicted */
	u32 max_io;		/* payload limit of one READ/WRITE, page multiple */
	u32 tree_flags;		/* OVNFS_HELLO_* */
	int active;		/* an operation ran since the last POLL */
	unsigned long state;	/* OVNFS_SB_* bits */
	struct delayed_work poll;

	/* The journal (journal.c): operations sent behind their callers. */
	spinlock_t jlock;	/* protects the journal and the id pool */
	struct list_head jlist;	/* entries not yet taken by the worker, oldest first */
	u64 jtail;		/* seq of the newest entry */
	u64 jdone;		/* every entry up to this seq has been answered */
	u32 jcount;		/* entries appended and not yet freed */
	size_t jbytes;		/* their memory */
	wait_queue_head_t jwait;	/* waiters for jdone and for room */
	struct work_struct jwork;
	struct ovnfs_batch *batch;

	/* Ids for files and directories created ahead of the daemon (ALLOC_IDS). */
	u64 id_next, id_end;	/* the run in use */
	u64 spare_next, spare_end;	/* the next run */
	unsigned int id_refilling;	/* id_work is queued or running */
	unsigned int ids_off;	/* the daemon cannot hand out ids */
	struct work_struct id_work;
};

/* ovnfs_sb.state: the POLL work is queued or running. */
#define OVNFS_SB_ARMED		0

/* ovnfs_inode.flags bits. */
#define OVNFS_I_STALE		0	/* the daemon answered ESTALE for this id */
#define OVNFS_I_ORPHAN		1	/* unlinked while open: the page cache is the only copy */
#define OVNFS_I_OWN		2	/* this guest changed the inode's attributes */
#define OVNFS_I_BAD		4	/* its journaled CREATE/MKDIR failed: no daemon file */
#define OVNFS_I_MTIME_LOCAL	5	/* a write stamped mtime/ctime here; daemon times don't replace them */
#define OVNFS_I_IN_BATCH	6	/* the journal worker holds folios of it in the current batch */

/* Per-inode state; the VFS inode is embedded. */
struct ovnfs_inode {
	struct inode vfs;
	u64 id;			/* daemon file id, also i_ino */
	u64 attr_gen;		/* generation the attributes were validated under */
	u64 own_seq;		/* attr_seq of this guest's last change (under i_lock) */
	u64 srv_size;		/* size and mtime the daemon last reported */
	s64 srv_mtime;
	unsigned long flags;	/* OVNFS_I_* bits */
	atomic_t opens;		/* open files (regular files) */
	u64 jseq;		/* seq of the newest journal entry naming this inode */
	u64 hold_seq;		/* seq of its newest CREATE or size SETATTR: data waits for it */
	loff_t zero_point;	/* the daemon's copy holds only zeros from here on (i_lock) */
	unsigned int wb_active;	/* writepages calls sending its data (jlock) */
	struct ovnfs_jent *data_ent;	/* queued closed-file write-back (under jlock) */

	/* Directories: the listing, complete while dir_gen is current. */
	struct mutex dir_lock;
	struct rb_root dir_ents;
	u32 dir_count;
	u64 dir_gen;
	u64 dir_nolist_gen;	/* listing refused (too big, unstable) under this gen */
	u64 dir_since;		/* attr_seq when the listing was taken */
	u64 dir_evicts;		/* evict_seq when the listing was taken */

	/* Symlinks: the target, published once, freed with the inode. */
	char *link;
	u64 link_gen;
};

/* Where attributes handed to ovnfs_apply_attr come from. */
enum ovnfs_attr_src {
	OVNFS_ATTR_NEW,		/* initializing a fresh inode */
	OVNFS_ATTR_REMOTE,	/* a refresh: may reveal changes made elsewhere */
	OVNFS_ATTR_OWN,		/* the reply to this guest's own mutation */
	OVNFS_ATTR_DATA,	/* the reply to write-back or a journaled operation: times stay */
};

/* Returns the ovnfs inode embedding a VFS inode. */
static inline struct ovnfs_inode *OVNFS_I(const struct inode *inode)
{
	return container_of(inode, struct ovnfs_inode, vfs);
}

/* Returns the ovnfs state of a superblock. */
static inline struct ovnfs_sb *OVNFS_SB(const struct super_block *sb)
{
	return sb->s_fs_info;
}

/* Turns a positive errno from the device or the daemon into a kernel error. */
static inline int ovnfs_errno(u32 err)
{
	return err < MAX_ERRNO ? -(int)err : -EIO;
}

/*
 * Says whether journal entries naming the inode are unanswered; while they
 * are, the inode's cached state is authoritative.
 */
static inline bool ovnfs_pending(const struct inode *inode)
{
	/* Acquire: pairs with the worker's release of jdone. */
	return READ_ONCE(OVNFS_I(inode)->jseq) > smp_load_acquire(&OVNFS_SB(inode->i_sb)->jdone);
}

/* Returns the current cache generation; safe in RCU walk. */
static inline u64 ovnfs_gen(const struct ovnfs_sb *sbi)
{
	return READ_ONCE(sbi->gen);
}

void ovnfs_arm_poll(struct ovnfs_sb *sbi);

/*
 * Records filesystem activity so the POLL work keeps running. Cheap enough
 * for RCU walk: the shared flag is written only on the idle-to-active edge.
 */
static inline void ovnfs_touch(struct ovnfs_sb *sbi)
{
	if (!READ_ONCE(sbi->active))
		ovnfs_arm_poll(sbi);
}

/* call.c */
int ovnfs_transport_init(void);
void ovnfs_transport_exit(void);
int ovnfs_call(struct ovnfs_sb *sbi, struct ovnfs_msg *m);

/* Kinds of journal entries; the order matches the journal_* counters. */
enum ovnfs_jop {
	OVNFS_J_CREATE,
	OVNFS_J_MKDIR,
	OVNFS_J_UNLINK,
	OVNFS_J_RENAME,
	OVNFS_J_LINK,
	OVNFS_J_RMDIR,
	OVNFS_J_SETATTR,
	OVNFS_J_DATA,		/* a closed file's dirty data */
	OVNFS_J_NR,
};

/* Event counters shown in debugfs besides the per-op calls. */
enum ovnfs_counter {
	OVNFS_C_BATCHED,	/* sub-requests sent inside COMPOUND */
	OVNFS_C_JOURNAL,	/* first of OVNFS_J_NR: entries journaled per kind */
	OVNFS_C_CREATE_DATA = OVNFS_C_JOURNAL + OVNFS_J_NR,	/* CREATEs carrying content */
	OVNFS_C_ID_REFILLS,
	OVNFS_C_LATE_FAILURES,
	OVNFS_C_NR,
};

void ovnfs_count(enum ovnfs_counter c, unsigned int n);
void ovnfs_note_journal_high(u32 entries);

/* journal.c */

/* A namespace operation for the journal, applied locally by the caller. */
struct ovnfs_jdesc {
	enum ovnfs_jop op;
	struct inode *dir;	/* the directory holding name */
	struct inode *dir2;	/* RENAME: the directory receiving name2 */
	struct inode *inode;	/* the node created, removed, moved or linked */
	struct inode *target;	/* RENAME: the node name2 replaced, or NULL */
	u32 mode;		/* CREATE, MKDIR, SETATTR: permission bits */
	u32 flags;		/* RENAME: OVNFS_RENAME_* */
	s64 mtime;		/* CREATE, MKDIR: the new node's mtime */
	u32 valid;		/* SETATTR: OVNFS_SET_* */
	u64 size;		/* SETATTR: the size set */
};

int ovnfs_journal_init(struct ovnfs_sb *sbi);
void ovnfs_journal_exit(struct ovnfs_sb *sbi);
struct ovnfs_jent *ovnfs_journal_prepare(struct ovnfs_sb *sbi, const struct qstr *name,
					 const struct qstr *name2);
void ovnfs_journal_commit(struct ovnfs_sb *sbi, struct ovnfs_jent *e, const struct ovnfs_jdesc *d);
void ovnfs_journal_commit_held(struct ovnfs_sb *sbi, struct ovnfs_jent *e,
			       const struct ovnfs_jdesc *d);
void ovnfs_journal_release(struct ovnfs_sb *sbi, struct ovnfs_jent *e);
void ovnfs_journal_abort(struct ovnfs_sb *sbi, struct ovnfs_jent *e);
int ovnfs_journal_data(struct inode *inode);
bool ovnfs_journal_drop_data(struct inode *inode);
void ovnfs_journal_wait_inode(struct inode *inode);
void ovnfs_journal_wait_nodes(struct inode *const *nodes, unsigned int n);
bool ovnfs_writeback_begin(struct inode *inode, bool wait);
void ovnfs_writeback_end(struct inode *inode);
void ovnfs_journal_drain(struct ovnfs_sb *sbi);
void ovnfs_journal_quiesce(struct ovnfs_sb *sbi);
void ovnfs_journal_kick(struct ovnfs_sb *sbi);
int ovnfs_sync_inode(struct inode *inode);
void ovnfs_ids_fill(struct ovnfs_sb *sbi);
u64 ovnfs_id_take(struct ovnfs_sb *sbi);

/* super.c */
u64 ovnfs_note_epoch(struct ovnfs_sb *sbi, u64 epoch);

/* inode.c */
int ovnfs_inode_cache_init(void);
void ovnfs_inode_cache_exit(void);
struct inode *ovnfs_alloc_inode(struct super_block *sb);
void ovnfs_free_inode(struct inode *inode);
void ovnfs_evict_inode(struct inode *inode);
struct inode *ovnfs_iget(struct super_block *sb, const struct ovnfs_attr *a, u64 gen,
			 enum ovnfs_attr_src src, u64 since);
struct inode *ovnfs_iget_id(struct super_block *sb, u64 id);
void ovnfs_apply_attr(struct inode *inode, const struct ovnfs_attr *a, u64 gen,
		      enum ovnfs_attr_src src, u64 since);
void ovnfs_attr_from_inode(struct inode *inode, struct ovnfs_attr *a);
void ovnfs_note_server(struct inode *inode, const struct ovnfs_attr *a, u64 since);
void ovnfs_apply_written(struct inode *inode, const struct ovnfs_attr *a, u64 gen, u64 since);
s64 ovnfs_mtime_req(struct inode *inode, struct ovnfs_setattr_req *req);
void ovnfs_mtime_synced(struct inode *inode, const struct ovnfs_attr *a, s64 sent);
int ovnfs_send_mtime(struct inode *inode);
void ovnfs_own_change(struct inode *inode);
bool ovnfs_attr_stale(struct inode *inode);
int ovnfs_refresh(struct inode *inode);
int ovnfs_revalidate_attr(struct inode *inode);
void ovnfs_mark_stale(struct inode *inode);
void ovnfs_writeback_error(struct inode *inode, int err);
int ovnfs_icall(struct inode *inode, struct ovnfs_msg *m);
int ovnfs_icall_now(struct inode *inode, struct ovnfs_msg *m);
bool ovnfs_attr_valid(struct inode *inode);
void ovnfs_unlinked(struct inode *inode);
void ovnfs_make_orphan(struct inode *inode);
void ovnfs_zero_from(struct inode *inode, loff_t pos);
void ovnfs_zero_past(struct inode *inode, loff_t end);
bool ovnfs_is_zero(struct inode *inode, loff_t pos);
bool ovnfs_link_publish(struct inode *inode, char *target, u64 gen);
int ovnfs_getattr(struct mnt_idmap *idmap, const struct path *path,
		  struct kstat *stat, u32 request_mask, unsigned int flags);
int ovnfs_setattr(struct mnt_idmap *idmap, struct dentry *dentry, struct iattr *ia);

extern const struct inode_operations ovnfs_file_iops;
extern const struct inode_operations ovnfs_symlink_iops;
extern const struct inode_operations ovnfs_special_iops;

/* dir.c */
void ovnfs_dir_drop_table(struct ovnfs_inode *di);
void ovnfs_forget_name(struct inode *dir, const char *name, u16 len);

extern const struct inode_operations ovnfs_dir_iops;
extern const struct file_operations ovnfs_dir_fops;
extern const struct dentry_operations ovnfs_dentry_ops;

/* file.c */
extern const struct file_operations ovnfs_file_fops;
extern const struct address_space_operations ovnfs_aops;

#endif /* OVNFS_H */
