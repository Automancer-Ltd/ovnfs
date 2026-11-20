// SPDX-License-Identifier: GPL-2.0-only
/*
 * super.c - module init, mount, superblock, statfs and the epoch poller.
 *
 * One superblock per guest (get_tree_single): two superblocks over the same
 * daemon would each miss the other's changes, because the daemon does not
 * change the epoch for this guest's own requests.
 */
#include <linux/backing-dev.h>
#include <linux/fs_context.h>
#include <linux/module.h>
#include <linux/slab.h>
#include <linux/statfs.h>
#include <linux/writeback.h>

#include "ovnfs.h"

/*
 * Maps a reply's epoch to the generation it belongs to, bumping the
 * generation when the epoch differs from the last one seen. Lock-free when
 * the epoch is unchanged: gen is read before epoch and written after it, so a
 * reader that sees a new gen also sees its epoch, and a reader racing an
 * update returns the older gen, which only makes its caller's entry look
 * stale sooner.
 */
u64 ovnfs_note_epoch(struct ovnfs_sb *sbi, u64 epoch)
{
	u64 gen = READ_ONCE(sbi->gen);

	/* Pairs with the writer's smp_wmb: a new gen implies its epoch. */
	smp_rmb();
	if (READ_ONCE(sbi->epoch) == epoch)
		return gen;
	spin_lock(&sbi->epoch_lock);
	if (sbi->epoch != epoch) {
		WRITE_ONCE(sbi->epoch, epoch);
		/* Pairs with the lock-free reader's smp_rmb. */
		smp_wmb();
		WRITE_ONCE(sbi->gen, sbi->gen + 1);
	}
	gen = sbi->gen;
	spin_unlock(&sbi->epoch_lock);
	return gen;
}

/*
 * Sends POLL once a second while the filesystem is active, so a change made
 * elsewhere is noticed within about a second even when every operation is
 * answered from cache; disarms itself after an idle second.
 */
static void ovnfs_poll_work(struct work_struct *work)
{
	struct ovnfs_sb *sbi = container_of(to_delayed_work(work), struct ovnfs_sb, poll);
	struct ovnfs_msg m = { .op = OVNFS_OP_POLL, .id = OVNFS_ROOT_ID };

	if (!xchg(&sbi->active, 0)) {
		clear_bit(OVNFS_SB_ARMED, &sbi->state);
		/* Pairs with ovnfs_arm_poll: either it sees ARMED clear or we see active. */
		smp_mb__after_atomic();
		if (READ_ONCE(sbi->active) && !test_and_set_bit(OVNFS_SB_ARMED, &sbi->state))
			queue_delayed_work(system_wq, &sbi->poll, 0);
		return;
	}
	/* Only the reply's epoch matters; a failed poll is retried next second. */
	ovnfs_call(sbi, &m);
	queue_delayed_work(system_wq, &sbi->poll, HZ);
}

/*
 * Marks the filesystem active and, coming out of idle, polls at once: the
 * cache may have gone unchecked for longer than a second.
 */
void ovnfs_arm_poll(struct ovnfs_sb *sbi)
{
	WRITE_ONCE(sbi->active, 1);
	/* Pairs with the poll work: either it sees active or we see ARMED clear. */
	smp_mb();
	if (!test_bit(OVNFS_SB_ARMED, &sbi->state) &&
	    !test_and_set_bit(OVNFS_SB_ARMED, &sbi->state))
		queue_delayed_work(system_wq, &sbi->poll, 0);
}

/*
 * Sends the journal; waits until it is answered when asked to (sync,
 * syncfs, unmount, remount read-only). Late failures are in s_wb_err.
 */
static int ovnfs_sync_fs(struct super_block *sb, int wait)
{
	struct ovnfs_sb *sbi = OVNFS_SB(sb);

	if (wait)
		ovnfs_journal_drain(sbi);
	else
		ovnfs_journal_kick(sbi);
	return 0;
}

/* Answers statfs with one STATFS call. */
static int ovnfs_statfs(struct dentry *dentry, struct kstatfs *buf)
{
	struct ovnfs_sb *sbi = OVNFS_SB(dentry->d_sb);
	struct ovnfs_statfs_rep r;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_STATFS,
		.id = OVNFS_ROOT_ID,
		.res = &r,
		.res_len = sizeof(r),
	};
	int err;

	err = ovnfs_call(sbi, &m);
	if (err)
		return err;
	buf->f_type = OVNFS_SUPER_MAGIC;
	buf->f_bsize = le32_to_cpu(r.bsize);
	buf->f_frsize = buf->f_bsize;
	buf->f_blocks = le64_to_cpu(r.blocks);
	buf->f_bfree = le64_to_cpu(r.bfree);
	buf->f_bavail = le64_to_cpu(r.bavail);
	buf->f_files = le64_to_cpu(r.files);
	buf->f_ffree = le64_to_cpu(r.ffree);
	buf->f_namelen = min_t(u32, le32_to_cpu(r.namelen) ?: OVNFS_NAME_MAX, OVNFS_NAME_MAX);
	return 0;
}

static const struct super_operations ovnfs_sops = {
	.alloc_inode = ovnfs_alloc_inode,
	.free_inode = ovnfs_free_inode,
	.evict_inode = ovnfs_evict_inode,
	.statfs = ovnfs_statfs,
	.sync_fs = ovnfs_sync_fs,
};

/*
 * Greets the daemon, sizes I/O, stocks the id pool for creates ahead of the
 * daemon and creates the root. Mount options are the generic ones: ro,
 * sync (data and namespace operations synchronous), dirsync (namespace
 * operations synchronous).
 */
static int ovnfs_fill_super(struct super_block *sb, struct fs_context *fc)
{
	struct ovnfs_hello_req req = { .version = cpu_to_le32(OVNFS_PROTO_VERSION) };
	struct ovnfs_hello_rep rep;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_HELLO,
		.id = OVNFS_ROOT_ID,
		.arg = &req,
		.arg_len = sizeof(req),
		.res = &rep,
		.res_len = sizeof(rep),
	};
	struct ovnfs_sb *sbi;
	struct inode *root;
	u32 max_io;
	int err;

	sbi = kzalloc(sizeof(*sbi), GFP_KERNEL);
	if (!sbi)
		return -ENOMEM;
	spin_lock_init(&sbi->epoch_lock);
	sbi->gen = 1;
	INIT_DELAYED_WORK(&sbi->poll, ovnfs_poll_work);
	sbi->sb = sb;
	sb->s_fs_info = sbi;
	err = ovnfs_journal_init(sbi);
	if (err)
		return err;

	err = ovnfs_call(sbi, &m);
	if (err)
		return err;
	if (le32_to_cpu(rep.version) != OVNFS_PROTO_VERSION) {
		pr_err("ovnfs: daemon speaks protocol %u\n", le32_to_cpu(rep.version));
		return -EPROTO;
	}
	max_io = min_t(u32, le32_to_cpu(rep.max_io), OVNFS_MAX_FOLIOS * PAGE_SIZE);
	max_io = round_down(max_io, PAGE_SIZE);
	if (!max_io || le64_to_cpu(rep.root.id) != OVNFS_ROOT_ID ||
	    !S_ISDIR(le32_to_cpu(rep.root.mode)))
		return -EPROTO;
	sbi->max_io = max_io;
	sbi->tree_flags = le32_to_cpu(rep.flags);
	if (sbi->tree_flags & OVNFS_HELLO_RDONLY)
		sb->s_flags |= SB_RDONLY;

	sb->s_magic = OVNFS_SUPER_MAGIC;
	sb->s_op = &ovnfs_sops;
	sb->s_d_op = &ovnfs_dentry_ops;
	sb->s_maxbytes = MAX_LFS_FILESIZE;
	sb->s_blocksize = PAGE_SIZE;
	sb->s_blocksize_bits = PAGE_SHIFT;
	sb->s_time_gran = 1;
	err = super_setup_bdi(sb);
	if (err)
		return err;
	sb->s_bdi->ra_pages = max_io >> PAGE_SHIFT;
	sb->s_bdi->io_pages = max_io >> PAGE_SHIFT;
	if (!sb_rdonly(sb))
		ovnfs_ids_fill(sbi);

	root = ovnfs_iget(sb, &rep.root, m.gen, OVNFS_ATTR_NEW, 0);
	if (IS_ERR(root))
		return PTR_ERR(root);
	sb->s_root = d_make_root(root);
	if (!sb->s_root)
		return -ENOMEM;
	return 0;
}

/* Mounts the guest's single ovnfs superblock. */
static int ovnfs_get_tree(struct fs_context *fc)
{
	return get_tree_single(fc, ovnfs_fill_super);
}

/*
 * Applies the generic flags on remount after draining the journal; a
 * read-only tree cannot become writable.
 */
static int ovnfs_reconfigure(struct fs_context *fc)
{
	struct super_block *sb = fc->root->d_sb;

	if (!(fc->sb_flags & SB_RDONLY) && (OVNFS_SB(sb)->tree_flags & OVNFS_HELLO_RDONLY))
		return -EROFS;
	return sync_filesystem(sb);
}

static const struct fs_context_operations ovnfs_context_ops = {
	.get_tree = ovnfs_get_tree,
	.reconfigure = ovnfs_reconfigure,
};

/* Sets up a mount context; all mount options are generic superblock flags. */
static int ovnfs_init_fs_context(struct fs_context *fc)
{
	fc->ops = &ovnfs_context_ops;
	return 0;
}

/*
 * Drains the journal and lets the worker drop its inode references while
 * the dcache still exists (late failures revisit dentries), tears the
 * superblock down, then stops the workers that use its state.
 */
static void ovnfs_kill_sb(struct super_block *sb)
{
	struct ovnfs_sb *sbi = OVNFS_SB(sb);

	if (sbi && sbi->batch)
		ovnfs_journal_quiesce(sbi);
	kill_anon_super(sb);
	if (sbi) {
		ovnfs_journal_exit(sbi);
		cancel_delayed_work_sync(&sbi->poll);
		kfree(sbi);
	}
}

static struct file_system_type ovnfs_fs_type = {
	.owner = THIS_MODULE,
	.name = "ovnfs",
	.init_fs_context = ovnfs_init_fs_context,
	.kill_sb = ovnfs_kill_sb,
};
MODULE_ALIAS_FS("ovnfs");

/* Probes the call device, then registers the filesystem. */
static int __init ovnfs_init(void)
{
	int err;

	err = ovnfs_transport_init();
	if (err)
		return err;
	err = ovnfs_inode_cache_init();
	if (err)
		goto out_transport;
	err = register_filesystem(&ovnfs_fs_type);
	if (err)
		goto out_cache;
	return 0;

out_cache:
	ovnfs_inode_cache_exit();
out_transport:
	ovnfs_transport_exit();
	return err;
}

/* Unregisters; the VFS holds a module reference while any mount exists. */
static void __exit ovnfs_exit(void)
{
	unregister_filesystem(&ovnfs_fs_type);
	ovnfs_inode_cache_exit();
	ovnfs_transport_exit();
}

module_init(ovnfs_init);
module_exit(ovnfs_exit);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("ovnfs: checkout filesystem over VMM traps");
