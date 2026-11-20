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
#include <linux/workqueue.h>

#include "ovnfs_wire.h"

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
	/* out */
	u32 status;		/* transport status: nonzero when no reply arrived */
	u32 data_got;		/* bulk bytes received */
	u64 gen;		/* generation of the reply's epoch */
};

/* Per-superblock state. */
struct ovnfs_sb {
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
};

/* ovnfs_sb.state: the POLL work is queued or running. */
#define OVNFS_SB_ARMED		0

/* ovnfs_inode.flags bits. */
#define OVNFS_I_STALE		0	/* the daemon answered ESTALE for this id */
#define OVNFS_I_ORPHAN		1	/* unlinked while open: the page cache is the only copy */
#define OVNFS_I_OWN		2	/* this guest changed the inode's attributes */

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
void ovnfs_own_change(struct inode *inode);
bool ovnfs_attr_stale(struct inode *inode);
int ovnfs_refresh(struct inode *inode);
int ovnfs_revalidate_attr(struct inode *inode);
void ovnfs_mark_stale(struct inode *inode);
int ovnfs_icall(struct inode *inode, struct ovnfs_msg *m);
void ovnfs_unlinked(struct inode *inode);
bool ovnfs_link_publish(struct inode *inode, char *target, u64 gen);
int ovnfs_getattr(struct mnt_idmap *idmap, const struct path *path,
		  struct kstat *stat, u32 request_mask, unsigned int flags);
int ovnfs_setattr(struct mnt_idmap *idmap, struct dentry *dentry, struct iattr *ia);

extern const struct inode_operations ovnfs_file_iops;
extern const struct inode_operations ovnfs_symlink_iops;
extern const struct inode_operations ovnfs_special_iops;

/* dir.c */
void ovnfs_dir_drop_table(struct ovnfs_inode *di);

extern const struct inode_operations ovnfs_dir_iops;
extern const struct file_operations ovnfs_dir_fops;
extern const struct dentry_operations ovnfs_dentry_ops;

/* file.c */
extern const struct file_operations ovnfs_file_fops;
extern const struct address_space_operations ovnfs_aops;

#endif /* OVNFS_H */
