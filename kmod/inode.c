// SPDX-License-Identifier: GPL-2.0-only
/*
 * inode.c - inode cache, attributes, setattr and symlinks.
 *
 * Inodes are keyed by daemon file id (iget5_locked, i_ino = id). Attributes
 * are valid while attr_gen equals the superblock's generation. A refresh that
 * shows a size or mtime this guest did not produce drops the page cache; the
 * guest's own changes (replies to its mutations) never do. Refresh replies
 * that were overtaken by an own change are discarded (own_seq vs. since), the
 * way FUSE guards with attr_version.
 */
#include <linux/dcache.h>
#include <linux/fs.h>
#include <linux/kdev_t.h>
#include <linux/mm.h>
#include <linux/namei.h>
#include <linux/pagemap.h>
#include <linux/slab.h>
#include <linux/time64.h>
#include <linux/writeback.h>

#include "ovnfs.h"

static struct kmem_cache *ovnfs_inode_cachep;

/* Initializes the parts of a cached inode object that survive reuse. */
static void ovnfs_inode_once(void *p)
{
	struct ovnfs_inode *oi = p;

	inode_init_once(&oi->vfs);
	mutex_init(&oi->dir_lock);
}

/* Creates the inode slab. */
int ovnfs_inode_cache_init(void)
{
	ovnfs_inode_cachep = kmem_cache_create("ovnfs_inode", sizeof(struct ovnfs_inode), 0,
					       SLAB_RECLAIM_ACCOUNT | SLAB_ACCOUNT,
					       ovnfs_inode_once);
	return ovnfs_inode_cachep ? 0 : -ENOMEM;
}

/* Destroys the inode slab once RCU-freed inodes are gone. */
void ovnfs_inode_cache_exit(void)
{
	rcu_barrier();
	kmem_cache_destroy(ovnfs_inode_cachep);
}

/* Allocates an inode with empty ovnfs state. */
struct inode *ovnfs_alloc_inode(struct super_block *sb)
{
	struct ovnfs_inode *oi = alloc_inode_sb(sb, ovnfs_inode_cachep, GFP_KERNEL);

	if (!oi)
		return NULL;
	oi->id = 0;
	oi->attr_gen = 0;
	oi->own_seq = 0;
	oi->srv_size = 0;
	oi->srv_mtime = 0;
	oi->flags = 0;
	atomic_set(&oi->opens, 0);
	oi->dir_ents = RB_ROOT;
	oi->dir_count = 0;
	oi->dir_gen = 0;
	oi->dir_nolist_gen = 0;
	oi->dir_since = 0;
	oi->dir_evicts = 0;
	oi->link = NULL;
	oi->link_gen = 0;
	return &oi->vfs;
}

/* Frees an inode after the RCU grace period, with its symlink target. */
void ovnfs_free_inode(struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);

	kfree(oi->link);
	kmem_cache_free(ovnfs_inode_cachep, oi);
}

/*
 * Drops an inode's pages and listing. Evicting an inode whose attributes
 * this guest changed makes every directory listing's attributes untrusted
 * for creating inodes (they may predate the change); names stay valid.
 */
void ovnfs_evict_inode(struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);

	truncate_inode_pages_final(&inode->i_data);
	clear_inode(inode);
	if (S_ISDIR(inode->i_mode))
		ovnfs_dir_drop_table(oi);
	if (test_bit(OVNFS_I_OWN, &oi->flags) && inode->i_nlink &&
	    !test_bit(OVNFS_I_ORPHAN, &oi->flags))
		atomic64_inc(&OVNFS_SB(inode->i_sb)->evict_seq);
}

/* iget5 match: same daemon id. */
static int ovnfs_inode_test(struct inode *inode, void *data)
{
	return OVNFS_I(inode)->id == *(const u64 *)data;
}

/* iget5 init: binds a new inode to its daemon id. */
static int ovnfs_inode_set(struct inode *inode, void *data)
{
	u64 id = *(const u64 *)data;

	OVNFS_I(inode)->id = id;
	inode->i_ino = id;
	return 0;
}

/* Says whether mode names a file type the protocol can describe. */
static bool ovnfs_mode_valid(umode_t mode)
{
	switch (mode & S_IFMT) {
	case S_IFREG:
	case S_IFDIR:
	case S_IFLNK:
	case S_IFIFO:
	case S_IFSOCK:
	case S_IFCHR:
	case S_IFBLK:
		return true;
	}
	return false;
}

/* Says whether the file has data that only this guest has (dirty or in flight). */
static bool ovnfs_has_local_data(struct inode *inode)
{
	struct address_space *mapping = inode->i_mapping;

	return mapping_tagged(mapping, PAGECACHE_TAG_DIRTY) ||
	       mapping_tagged(mapping, PAGECACHE_TAG_WRITEBACK);
}

/* Records an attribute change made by this guest; i_lock held. */
static void ovnfs_own_change_locked(struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);

	oi->own_seq = atomic64_inc_return(&OVNFS_SB(inode->i_sb)->attr_seq);
	set_bit(OVNFS_I_OWN, &oi->flags);
}

/* Records an attribute change made locally by this guest (link counts, dir times). */
void ovnfs_own_change(struct inode *inode)
{
	spin_lock(&inode->i_lock);
	ovnfs_own_change_locked(inode);
	spin_unlock(&inode->i_lock);
}

/*
 * Installs daemon attributes. While the file has dirty or in-flight pages
 * its local size is authoritative and is never shrunk. A REMOTE refresh is
 * discarded when this guest changed the inode after the refresh was sent
 * (own_seq > since), and drops the page cache when the size or mtime differs
 * from what the daemon last reported.
 */
void ovnfs_apply_attr(struct inode *inode, const struct ovnfs_attr *a, u64 gen,
		      enum ovnfs_attr_src src, u64 since)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);
	umode_t mode = le32_to_cpu(a->mode);
	u64 size = le64_to_cpu(a->size);
	s64 mtime = le64_to_cpu(a->mtime_ns);
	bool drop = false;
	bool local;

	if (src != OVNFS_ATTR_NEW && ((inode->i_mode ^ mode) & S_IFMT)) {
		ovnfs_mark_stale(inode);
		return;
	}
	spin_lock(&inode->i_lock);
	if (test_bit(OVNFS_I_ORPHAN, &oi->flags) ||
	    (src == OVNFS_ATTR_REMOTE && oi->own_seq > since)) {
		spin_unlock(&inode->i_lock);
		return;
	}
	local = S_ISREG(mode) && ovnfs_has_local_data(inode);
	if (src == OVNFS_ATTR_REMOTE && S_ISREG(mode) && !local &&
	    (size != oi->srv_size || mtime != oi->srv_mtime))
		drop = true;
	oi->srv_size = size;
	oi->srv_mtime = mtime;

	inode->i_mode = mode;
	set_nlink(inode, le32_to_cpu(a->nlink));
	i_uid_write(inode, le32_to_cpu(a->uid));
	i_gid_write(inode, le32_to_cpu(a->gid));
	if (!local || size > i_size_read(inode))
		i_size_write(inode, size);
	inode->i_blocks = le64_to_cpu(a->blocks);
	inode_set_mtime_to_ts(inode, ns_to_timespec64(mtime));
	inode_set_ctime_to_ts(inode, ns_to_timespec64(le64_to_cpu(a->ctime_ns)));
	if (src == OVNFS_ATTR_NEW)
		inode_set_atime_to_ts(inode, ns_to_timespec64(mtime));
	if (src == OVNFS_ATTR_OWN)
		ovnfs_own_change_locked(inode);
	WRITE_ONCE(oi->attr_gen, gen);
	spin_unlock(&inode->i_lock);

	if (drop)
		invalidate_inode_pages2(inode->i_mapping);
}

/* Fills wire attributes from the cached inode (for directory listings). */
void ovnfs_attr_from_inode(struct inode *inode, struct ovnfs_attr *a)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct timespec64 ctime;

	memset(a, 0, sizeof(*a));
	spin_lock(&inode->i_lock);
	a->id = cpu_to_le64(oi->id);
	a->size = cpu_to_le64(oi->srv_size);
	a->blocks = cpu_to_le64(inode->i_blocks);
	a->mtime_ns = cpu_to_le64(oi->srv_mtime);
	ctime = inode_get_ctime(inode);
	a->ctime_ns = cpu_to_le64(timespec64_to_ns(&ctime));
	a->mode = cpu_to_le32(inode->i_mode);
	a->nlink = cpu_to_le32(inode->i_nlink);
	a->uid = cpu_to_le32(i_uid_read(inode));
	a->gid = cpu_to_le32(i_gid_read(inode));
	a->rdev = cpu_to_le32(new_encode_dev(inode->i_rdev));
	spin_unlock(&inode->i_lock);
}

/* Sets operations by file type and installs the first attributes. */
static void ovnfs_init_inode(struct inode *inode, const struct ovnfs_attr *a, u64 gen)
{
	umode_t mode = le32_to_cpu(a->mode);

	inode->i_mode = mode;
	switch (mode & S_IFMT) {
	case S_IFREG:
		inode->i_op = &ovnfs_file_iops;
		inode->i_fop = &ovnfs_file_fops;
		inode->i_mapping->a_ops = &ovnfs_aops;
		break;
	case S_IFDIR:
		inode->i_op = &ovnfs_dir_iops;
		inode->i_fop = &ovnfs_dir_fops;
		break;
	case S_IFLNK:
		inode->i_op = &ovnfs_symlink_iops;
		break;
	default:
		inode->i_op = &ovnfs_special_iops;
		init_special_inode(inode, mode, new_decode_dev(le32_to_cpu(a->rdev)));
		break;
	}
	ovnfs_apply_attr(inode, a, gen, OVNFS_ATTR_NEW, 0);
}

/*
 * Returns the inode for the attributes a reply (or a listing) gave, creating
 * it or updating a cached one. Attributes of a cached inode already valid
 * under gen are kept: they are at least as new. An id whose cached inode has
 * another file type was reused (daemon restart): the old inode is unhashed
 * and a new one created.
 */
struct inode *ovnfs_iget(struct super_block *sb, const struct ovnfs_attr *a, u64 gen,
			 enum ovnfs_attr_src src, u64 since)
{
	u64 id = le64_to_cpu(a->id);
	umode_t mode = le32_to_cpu(a->mode);
	struct inode *inode;
	int tries;

	if (!id || !ovnfs_mode_valid(mode))
		return ERR_PTR(-EIO);
	for (tries = 0; tries < 2; tries++) {
		inode = iget5_locked(sb, (unsigned long)id, ovnfs_inode_test, ovnfs_inode_set, &id);
		if (!inode)
			return ERR_PTR(-ENOMEM);
		if (inode->i_state & I_NEW) {
			ovnfs_init_inode(inode, a, gen);
			unlock_new_inode(inode);
			return inode;
		}
		if (!((inode->i_mode ^ mode) & S_IFMT)) {
			if (src == OVNFS_ATTR_OWN || READ_ONCE(OVNFS_I(inode)->attr_gen) != gen)
				ovnfs_apply_attr(inode, a, gen, src, since);
			return inode;
		}
		ovnfs_mark_stale(inode);
		remove_inode_hash(inode);
		iput(inode);
	}
	return ERR_PTR(-ESTALE);
}

/* Returns the cached inode for id, or creates it from one GETATTR. */
struct inode *ovnfs_iget_id(struct super_block *sb, u64 id)
{
	struct ovnfs_sb *sbi = OVNFS_SB(sb);
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_GETATTR,
		.id = id,
		.res = &a,
		.res_len = sizeof(a),
	};
	struct inode *inode;
	u64 since;
	int err;

	inode = ilookup5(sb, (unsigned long)id, ovnfs_inode_test, &id);
	if (inode)
		return inode;
	since = atomic64_read(&sbi->attr_seq);
	err = ovnfs_call(sbi, &m);
	if (err)
		return ERR_PTR(err);
	if (le64_to_cpu(a.id) != id)
		return ERR_PTR(-EIO);
	return ovnfs_iget(sb, &a, m.gen, OVNFS_ATTR_REMOTE, since);
}

/*
 * Marks an inode whose id the daemon no longer knows: its attributes and
 * every dentry naming it become stale and the parents' listings incomplete,
 * so the path walk the VFS retries on ESTALE looks the names up again.
 */
void ovnfs_mark_stale(struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct dentry *dentry;

	set_bit(OVNFS_I_STALE, &oi->flags);
	WRITE_ONCE(oi->attr_gen, 0);
	spin_lock(&inode->i_lock);
	hlist_for_each_entry(dentry, &inode->i_dentry, d_u.d_alias) {
		struct dentry *parent;

		spin_lock(&dentry->d_lock);
		WRITE_ONCE(dentry->d_time, 0);
		parent = dentry->d_parent;
		if (parent != dentry && d_really_is_positive(parent))
			WRITE_ONCE(OVNFS_I(d_inode(parent))->dir_gen, 0);
		spin_unlock(&dentry->d_lock);
	}
	spin_unlock(&inode->i_lock);
}

/*
 * Issues a call addressed to inode. ESTALE marks the inode stale; a call
 * whose reply never arrived may still have run, so the inode's attributes
 * are fetched again on next use.
 */
int ovnfs_icall(struct inode *inode, struct ovnfs_msg *m)
{
	int err;

	m->id = OVNFS_I(inode)->id;
	err = ovnfs_call(OVNFS_SB(inode->i_sb), m);
	if (err == -ESTALE)
		ovnfs_mark_stale(inode);
	else if (m->status)
		WRITE_ONCE(OVNFS_I(inode)->attr_gen, 0);
	return err;
}

/* Says whether the inode's attributes need a refresh before use. */
bool ovnfs_attr_stale(struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);

	return !test_bit(OVNFS_I_ORPHAN, &oi->flags) &&
	       READ_ONCE(oi->attr_gen) != ovnfs_gen(OVNFS_SB(inode->i_sb));
}

/* Refreshes attributes with one GETATTR. */
int ovnfs_refresh(struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_GETATTR,
		.res = &a,
		.res_len = sizeof(a),
	};
	u64 since;
	int err;

	if (test_bit(OVNFS_I_ORPHAN, &oi->flags))
		return 0;
	since = atomic64_read(&OVNFS_SB(inode->i_sb)->attr_seq);
	err = ovnfs_icall(inode, &m);
	if (err)
		return err;
	if (le64_to_cpu(a.id) != oi->id)
		return -EIO;
	ovnfs_apply_attr(inode, &a, m.gen, OVNFS_ATTR_REMOTE, since);
	return 0;
}

/* Refreshes attributes only when they are stale. */
int ovnfs_revalidate_attr(struct inode *inode)
{
	return ovnfs_attr_stale(inode) ? ovnfs_refresh(inode) : 0;
}

/*
 * Accounts for a name of inode that this guest removed. An open file whose
 * id the daemon dropped becomes an orphan: its pages are its only store, kept
 * resident (unevictable) and never written back.
 */
void ovnfs_unlinked(struct inode *inode)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_GETATTR,
		.id = oi->id,
		.res = &a,
		.res_len = sizeof(a),
	};
	int err = -ENOENT;

	if (S_ISREG(inode->i_mode) && atomic_read(&oi->opens))
		err = ovnfs_call(OVNFS_SB(inode->i_sb), &m);
	if (err == -ESTALE) {
		set_bit(OVNFS_I_ORPHAN, &oi->flags);
		mapping_set_unevictable(inode->i_mapping);
		clear_nlink(inode);
		return;
	}
	if (!err && le64_to_cpu(a.id) == oi->id) {
		ovnfs_apply_attr(inode, &a, m.gen, OVNFS_ATTR_OWN, 0);
		return;
	}
	if (inode->i_nlink)
		drop_nlink(inode);
	ovnfs_own_change(inode);
}

/* Answers stat, refreshing stale attributes with one GETATTR. */
int ovnfs_getattr(struct mnt_idmap *idmap, const struct path *path,
		  struct kstat *stat, u32 request_mask, unsigned int flags)
{
	struct inode *inode = d_inode(path->dentry);
	unsigned int sync = flags & AT_STATX_SYNC_TYPE;
	int err = 0;

	ovnfs_touch(OVNFS_SB(inode->i_sb));
	if (sync == AT_STATX_FORCE_SYNC)
		err = ovnfs_refresh(inode);
	else if (sync != AT_STATX_DONT_SYNC)
		err = ovnfs_revalidate_attr(inode);
	if (err)
		return err;
	generic_fillattr(idmap, request_mask, inode, stat);
	return 0;
}

/* Sets the size this guest decided on (truncate), regardless of local data. */
static void ovnfs_set_size(struct inode *inode, loff_t size)
{
	spin_lock(&inode->i_lock);
	i_size_write(inode, size);
	spin_unlock(&inode->i_lock);
}

/*
 * Changes attributes with one SETATTR. Truncation writes dirty pages back
 * first, then sends SETATTR, then adjusts the page cache. An orphan has no
 * daemon state and changes locally only.
 */
int ovnfs_setattr(struct mnt_idmap *idmap, struct dentry *dentry, struct iattr *ia)
{
	struct inode *inode = d_inode(dentry);
	struct ovnfs_setattr_req req = {};
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_SETATTR,
		.arg = &req,
		.arg_len = sizeof(req),
		.res = &a,
		.res_len = sizeof(a),
	};
	loff_t oldsize = i_size_read(inode);
	u32 valid = 0;
	int err;

	err = setattr_prepare(idmap, dentry, ia);
	if (err)
		return err;
	if (ia->ia_valid & ATTR_SIZE) {
		err = filemap_write_and_wait(inode->i_mapping);
		if (err)
			return err;
		valid |= OVNFS_SET_SIZE;
		req.size = cpu_to_le64(ia->ia_size);
	}
	if (ia->ia_valid & ATTR_MODE) {
		valid |= OVNFS_SET_MODE;
		req.mode = cpu_to_le32(ia->ia_mode & S_IALLUGO);
	}
	if (ia->ia_valid & ATTR_UID) {
		kuid_t uid = from_vfsuid(idmap, i_user_ns(inode), ia->ia_vfsuid);

		valid |= OVNFS_SET_UID;
		req.uid = cpu_to_le32(from_kuid(&init_user_ns, uid));
	}
	if (ia->ia_valid & ATTR_GID) {
		kgid_t gid = from_vfsgid(idmap, i_user_ns(inode), ia->ia_vfsgid);

		valid |= OVNFS_SET_GID;
		req.gid = cpu_to_le32(from_kgid(&init_user_ns, gid));
	}
	if (ia->ia_valid & ATTR_MTIME) {
		if (ia->ia_valid & ATTR_MTIME_SET) {
			valid |= OVNFS_SET_MTIME;
			req.mtime_ns = cpu_to_le64(timespec64_to_ns(&ia->ia_mtime));
		} else {
			valid |= OVNFS_SET_MTIME_NOW;
		}
	}

	if (!valid || test_bit(OVNFS_I_ORPHAN, &OVNFS_I(inode)->flags)) {
		if (ia->ia_valid & ATTR_SIZE)
			truncate_setsize(inode, ia->ia_size);
		setattr_copy(idmap, inode, ia);
		return 0;
	}

	req.valid = cpu_to_le32(valid);
	err = ovnfs_icall(inode, &m);
	if (err)
		return err;
	if (le64_to_cpu(a.id) != OVNFS_I(inode)->id)
		return -EIO;
	ovnfs_apply_attr(inode, &a, m.gen, OVNFS_ATTR_OWN, 0);
	if (ia->ia_valid & ATTR_ATIME)
		inode_set_atime_to_ts(inode, ia->ia_atime);
	if (ia->ia_valid & ATTR_SIZE) {
		ovnfs_set_size(inode, ia->ia_size);
		if (ia->ia_size > oldsize)
			pagecache_isize_extended(inode, oldsize, ia->ia_size);
		truncate_pagecache(inode, ia->ia_size);
	}
	return 0;
}

/*
 * Publishes a symlink target read or written under gen. The first target
 * stays for the inode's lifetime, so RCU walkers and legitimized walks may
 * keep using it; a later different target (id reuse) is not cached. Returns
 * false, leaving target to the caller, when a target is already cached.
 */
bool ovnfs_link_publish(struct inode *inode, char *target, u64 gen)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);

	/* Release: a reader that sees the pointer sees the string. */
	if (cmpxchg_release(&oi->link, NULL, target))
		return false;
	WRITE_ONCE(oi->link_gen, gen);
	return true;
}

/*
 * Returns a symlink's target: from the cache when it was validated under the
 * current generation (RCU walk included), else with one READLINK.
 */
static const char *ovnfs_get_link(struct dentry *dentry, struct inode *inode,
				  struct delayed_call *done)
{
	struct ovnfs_inode *oi = OVNFS_I(inode);
	/* Acquire: pairs with the publisher's release. */
	char *link = smp_load_acquire(&oi->link);
	struct ovnfs_msg m = { .op = OVNFS_OP_READLINK };
	char *buf;
	int err;

	if (link && READ_ONCE(oi->link_gen) == ovnfs_gen(OVNFS_SB(inode->i_sb)))
		return link;
	if (!dentry)
		return ERR_PTR(-ECHILD);

	buf = kmalloc(PATH_MAX, GFP_KERNEL);
	if (!buf)
		return ERR_PTR(-ENOMEM);
	m.buf = buf;
	m.buf_len = PATH_MAX - 1;
	err = ovnfs_icall(inode, &m);
	if (!err && (!m.data_got || memchr(buf, 0, m.data_got)))
		err = -EIO;
	if (err) {
		kfree(buf);
		return ERR_PTR(err);
	}
	buf[m.data_got] = 0;

	if (!link) {
		if (ovnfs_link_publish(inode, buf, m.gen))
			return buf;
		/* Lost the race to another publisher: compare with its target. */
		link = smp_load_acquire(&oi->link);
	}
	if (!strcmp(link, buf)) {
		WRITE_ONCE(oi->link_gen, m.gen);
		kfree(buf);
		return link;
	}
	set_delayed_call(done, kfree_link, buf);
	return buf;
}

const struct inode_operations ovnfs_file_iops = {
	.getattr = ovnfs_getattr,
	.setattr = ovnfs_setattr,
};

const struct inode_operations ovnfs_symlink_iops = {
	.get_link = ovnfs_get_link,
	.getattr = ovnfs_getattr,
	.setattr = ovnfs_setattr,
};

const struct inode_operations ovnfs_special_iops = {
	.getattr = ovnfs_getattr,
	.setattr = ovnfs_setattr,
};
