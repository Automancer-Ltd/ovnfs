// SPDX-License-Identifier: GPL-2.0-only
/*
 * dir.c - lookup, listings, dcache priming, namespace mutations, dentry ops.
 *
 * Every directory may hold its listing (an rbtree of name -> attributes)
 * that is complete while dir_gen equals the superblock's generation. A
 * complete listing answers lookups, hits and misses alike, without a call;
 * lookup in a directory not listed under the current generation lists it
 * first (one READDIR sequence instead of one LOOKUP per name). The guest's
 * own namespace mutations edit the listing and the dcache from their replies
 * and keep the directory complete.
 *
 * Lock order (outermost first):
 *   1. VFS i_rwsem of directories (shared: lookup, readdir; exclusive:
 *      mutations; two directories: lock_rename order)
 *   2. in-lookup dentries (d_alloc_parallel waits for them)
 *   3. ovnfs_inode.dir_lock of one directory
 *   4. inode i_lock, then dentry d_lock (the dcache's order); port lock
 * Rules that keep this acyclic:
 *   - dir_lock is never held while waiting for a dentry, taking an i_rwsem,
 *     taking another directory's dir_lock or locking a folio. READDIR calls
 *     run under it.
 *   - Only iterate_shared primes the dcache from a listing, holding no
 *     in-lookup dentry and no dir_lock. ->lookup never primes: a lookup
 *     holds its own in-lookup dentry, so priming from it would wait for
 *     other names that concurrent lookups hold in-lookup while they wait for
 *     this lookup's listing (or its name), a deadlock. A listing made by
 *     lookup fills the table only, and later lookups answer from it.
 *   - Page-cache invalidation after a remote change runs without dir_lock
 *     and without folio locks.
 */
#include <linux/dcache.h>
#include <linux/fs.h>
#include <linux/fs_types.h>
#include <linux/kdev_t.h>
#include <linux/mm.h>
#include <linux/namei.h>
#include <linux/rbtree.h>
#include <linux/slab.h>
#include <linux/stringhash.h>
#include <linux/wait.h>

#include "ovnfs.h"

/* Largest listing a directory keeps; bigger ones answer lookups with LOOKUP. */
#define OVNFS_DIR_MAX_ENTRIES	(1U << 17)
/* First READDIR reply budget; later pages grow up to max_io. */
#define OVNFS_READDIR_FIRST	(16U << 10)
/* Listings restarted because the epoch changed mid-way before giving up. */
#define OVNFS_LIST_TRIES	3

/* One entry of a directory's listing. */
struct ovnfs_dent {
	struct rb_node node;
	struct ovnfs_attr attr;
	u16 len;
	char name[];
};

/* Answer of a listing or a LOOKUP for one name. */
struct ovnfs_entry {
	struct ovnfs_attr attr;
	u64 gen;		/* generation the answer is valid under */
	u64 since;		/* attr_seq when the answer was produced */
	int found;		/* the name exists */
	int trusted;		/* attr may initialize a new inode */
};

/* How ovnfs_dir_ready may obtain a listing. */
enum ovnfs_listing_use {
	OVNFS_USE_CACHED,	/* only a current listing */
	OVNFS_USE_FILL,		/* list the directory when needed */
};

/* Facts about one READDIR sequence. */
struct ovnfs_listing {
	u64 gen;		/* generation of the first page */
	u64 since;		/* attr_seq before the first page */
	u64 evicts;		/* evict_seq before the first page */
	int mixed;		/* pages came from different generations */
};

/* Receives one entry of a listing, in ascending name order. */
typedef int (*ovnfs_list_fn)(void *ctx, const struct ovnfs_attr *a, const char *name, u16 len);

/* Orders names as the daemon does: bytewise, a prefix first. */
static int ovnfs_name_cmp(const char *a, u16 alen, const char *b, u16 blen)
{
	int c = memcmp(a, b, min(alen, blen));

	if (c)
		return c;
	return alen < blen ? -1 : alen > blen;
}

/* Finds name in a directory's listing; dir_lock held. */
static struct ovnfs_dent *ovnfs_dent_find(struct ovnfs_inode *di, const char *name, u16 len)
{
	struct rb_node *n = di->dir_ents.rb_node;

	while (n) {
		struct ovnfs_dent *d = rb_entry(n, struct ovnfs_dent, node);
		int c = ovnfs_name_cmp(name, len, d->name, d->len);

		if (!c)
			return d;
		n = c < 0 ? n->rb_left : n->rb_right;
	}
	return NULL;
}

/* Links a new entry into a tree; returns the existing one on a duplicate. */
static struct ovnfs_dent *ovnfs_dent_insert(struct rb_root *root, struct ovnfs_dent *d)
{
	struct rb_node **p = &root->rb_node, *parent = NULL;

	while (*p) {
		struct ovnfs_dent *cur = rb_entry(*p, struct ovnfs_dent, node);
		int c = ovnfs_name_cmp(d->name, d->len, cur->name, cur->len);

		if (!c)
			return cur;
		parent = *p;
		p = c < 0 ? &(*p)->rb_left : &(*p)->rb_right;
	}
	rb_link_node(&d->node, parent, p);
	rb_insert_color(&d->node, root);
	return NULL;
}

/* Allocates a listing entry. */
static struct ovnfs_dent *ovnfs_dent_alloc(const struct ovnfs_attr *a, const char *name, u16 len)
{
	struct ovnfs_dent *d = kmalloc(struct_size(d, name, len), GFP_KERNEL);

	if (!d)
		return NULL;
	d->attr = *a;
	d->len = len;
	memcpy(d->name, name, len);
	return d;
}

/* Frees every entry of a tree. */
static void ovnfs_dents_free(struct rb_root *root)
{
	struct ovnfs_dent *d, *next;

	rbtree_postorder_for_each_entry_safe(d, next, root, node)
		kfree(d);
	*root = RB_ROOT;
}

/* Discards a directory's listing; dir_lock held or the inode is dying. */
void ovnfs_dir_drop_table(struct ovnfs_inode *di)
{
	ovnfs_dents_free(&di->dir_ents);
	di->dir_count = 0;
	WRITE_ONCE(di->dir_gen, 0);
}

/* Checks a listed name: no NUL or '/', not "." or "..". */
static bool ovnfs_name_valid(const char *name, u16 len)
{
	if (!len || len > OVNFS_NAME_MAX || memchr(name, 0, len) || memchr(name, '/', len))
		return false;
	return !(name[0] == '.' && (len == 1 || (len == 2 && name[1] == '.')));
}

/*
 * Lists a directory with READDIR pages and hands every entry to fn, after
 * checking it lies inside the reply, is well formed and sorts after the
 * previous one. Reply budgets start small and grow towards max_io.
 */
static int ovnfs_list(struct inode *dir, ovnfs_list_fn fn, void *ctx, struct ovnfs_listing *l)
{
	struct ovnfs_sb *sbi = OVNFS_SB(dir->i_sb);
	u32 size = min(OVNFS_READDIR_FIRST, sbi->max_io);
	char after[OVNFS_NAME_MAX];
	u16 after_len = 0;
	int first = 1;
	void *buf;
	int err;

	l->gen = 0;
	l->since = atomic64_read(&sbi->attr_seq);
	l->evicts = atomic64_read(&sbi->evict_seq);
	l->mixed = 0;
	buf = kvmalloc(size, GFP_KERNEL);
	if (!buf)
		return -ENOMEM;
	for (;;) {
		struct ovnfs_readdir_req req = {
			.max_bytes = cpu_to_le32(size),
			.after_len = cpu_to_le16(after_len),
		};
		struct ovnfs_readdir_rep rep;
		struct ovnfs_msg m = {
			.op = OVNFS_OP_READDIR,
			.arg = &req,
			.arg_len = sizeof(req),
			.name = after,
			.name_len = after_len,
			.res = &rep,
			.res_len = sizeof(rep),
			.buf = buf,
			.buf_len = size,
		};
		u32 count, i, off = 0;

		err = ovnfs_icall(dir, &m);
		if (err)
			break;
		if (first)
			l->gen = m.gen;
		else if (m.gen != l->gen)
			l->mixed = 1;
		first = 0;

		count = le32_to_cpu(rep.count);
		if (!count && !rep.eof) {
			err = -EIO;
			break;
		}
		for (i = 0; i < count; i++) {
			const size_t head = offsetof(struct ovnfs_dirent, name);
			const struct ovnfs_dirent *d = buf + off;
			u16 len;

			if (m.data_got - off < head) {
				err = -EIO;
				break;
			}
			len = le16_to_cpu(d->name_len);
			if (m.data_got - off < head + len ||
			    !ovnfs_name_valid((const char *)d->name, len) ||
			    (after_len &&
			     ovnfs_name_cmp((const char *)d->name, len, after, after_len) <= 0)) {
				err = -EIO;
				break;
			}
			err = fn(ctx, &d->attr, (const char *)d->name, len);
			if (err)
				break;
			memcpy(after, d->name, len);
			after_len = len;
			off += min_t(u32, ALIGN(head + len, 8), m.data_got - off);
		}
		if (err || rep.eof)
			break;
		if (size < sbi->max_io) {
			kvfree(buf);
			size = min(size * 4, sbi->max_io);
			buf = kvmalloc(size, GFP_KERNEL);
			if (!buf)
				return -ENOMEM;
		}
	}
	kvfree(buf);
	return err;
}

/* A listing being collected into a tree. */
struct ovnfs_fill {
	struct rb_root root;
	u32 count;
};

/* ovnfs_list sink building a directory's table. */
static int ovnfs_fill_add(void *ctx, const struct ovnfs_attr *a, const char *name, u16 len)
{
	struct ovnfs_fill *f = ctx;
	struct ovnfs_dent *d;

	if (f->count == OVNFS_DIR_MAX_ENTRIES)
		return -E2BIG;
	d = ovnfs_dent_alloc(a, name, len);
	if (!d)
		return -ENOMEM;
	if (ovnfs_dent_insert(&f->root, d)) {
		kfree(d);
		return -EIO;
	}
	f->count++;
	return 0;
}

/*
 * Replaces a directory's listing with a fresh one; dir_lock held. A
 * directory that is too big or keeps changing while listed is marked not to
 * be listed again under this generation (-ENODATA).
 */
static int ovnfs_dir_fill(struct inode *dir)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	struct ovnfs_fill f = { .root = RB_ROOT };
	struct ovnfs_listing l;
	int tries, err;

	for (tries = 0; tries < OVNFS_LIST_TRIES; tries++) {
		err = ovnfs_list(dir, ovnfs_fill_add, &f, &l);
		if (err || !l.mixed)
			break;
		ovnfs_dents_free(&f.root);
		f.count = 0;
		err = -EAGAIN;
	}
	if (err) {
		ovnfs_dents_free(&f.root);
		if (err != -E2BIG && err != -EAGAIN)
			return err;
		WRITE_ONCE(di->dir_nolist_gen, ovnfs_gen(OVNFS_SB(dir->i_sb)));
		return -ENODATA;
	}
	ovnfs_dir_drop_table(di);
	di->dir_ents = f.root;
	di->dir_count = f.count;
	di->dir_since = l.since;
	di->dir_evicts = l.evicts;
	WRITE_ONCE(di->dir_gen, l.gen);
	return 0;
}

/*
 * Makes sure the directory's listing can answer; dir_lock held. Returns 0
 * when it can, -ENODATA when no current listing exists (or may be made), or
 * the error of the listing.
 */
static int ovnfs_dir_ready(struct inode *dir, enum ovnfs_listing_use use)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	u64 gen = ovnfs_gen(OVNFS_SB(dir->i_sb));

	if (di->dir_gen == gen)
		return 0;
	if (use == OVNFS_USE_CACHED || di->dir_nolist_gen == gen)
		return -ENODATA;
	return ovnfs_dir_fill(dir);
}

/* Fills an answer from a ready listing; dir_lock held. */
static void ovnfs_dir_answer(struct inode *dir, const struct qstr *name, struct ovnfs_entry *e)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	struct ovnfs_dent *d = ovnfs_dent_find(di, name->name, name->len);

	e->gen = di->dir_gen;
	e->since = di->dir_since;
	e->trusted = di->dir_evicts == atomic64_read(&OVNFS_SB(dir->i_sb)->evict_seq);
	e->found = !!d;
	if (d)
		e->attr = d->attr;
}

/*
 * Answers name from the directory's listing. Returns -ENODATA when the
 * directory has no current listing and none may be made.
 */
static int ovnfs_dir_search(struct inode *dir, const struct qstr *name,
			    struct ovnfs_entry *e, enum ovnfs_listing_use use)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	int err;

	mutex_lock(&di->dir_lock);
	err = ovnfs_dir_ready(dir, use);
	if (!err)
		ovnfs_dir_answer(dir, name, e);
	mutex_unlock(&di->dir_lock);
	return err;
}

/* Answers name with one LOOKUP; a missing name is an answer, not an error. */
static int ovnfs_lookup_call(struct inode *dir, const struct qstr *name, struct ovnfs_entry *e)
{
	struct ovnfs_msg m = {
		.op = OVNFS_OP_LOOKUP,
		.name = name->name,
		.name_len = name->len,
		.res = &e->attr,
		.res_len = sizeof(e->attr),
	};
	int err;

	e->since = atomic64_read(&OVNFS_SB(dir->i_sb)->attr_seq);
	e->trusted = 1;
	err = ovnfs_icall(dir, &m);
	e->gen = m.gen;
	e->found = !err;
	return err == -ENOENT ? 0 : err;
}

/* Returns the inode an answer names, fetching attributes the listing cannot vouch for. */
static struct inode *ovnfs_entry_inode(struct super_block *sb, const struct ovnfs_entry *e)
{
	if (e->trusted)
		return ovnfs_iget(sb, &e->attr, e->gen, OVNFS_ATTR_REMOTE, e->since);
	return ovnfs_iget_id(sb, le64_to_cpu(e->attr.id));
}

/*
 * Looks name up: from the directory's listing (made now when the directory
 * was not listed under the current generation), else with one LOOKUP.
 */
static struct dentry *ovnfs_lookup(struct inode *dir, struct dentry *dentry, unsigned int flags)
{
	struct ovnfs_entry e;
	struct inode *inode = NULL;
	struct dentry *alias;
	int err;

	if (dentry->d_name.len > OVNFS_NAME_MAX)
		return ERR_PTR(-ENAMETOOLONG);
	ovnfs_touch(OVNFS_SB(dir->i_sb));
	err = ovnfs_dir_search(dir, &dentry->d_name, &e, OVNFS_USE_FILL);
	if (err == -ENODATA)
		err = ovnfs_lookup_call(dir, &dentry->d_name, &e);
	if (err)
		return ERR_PTR(err);
	if (e.found) {
		inode = ovnfs_entry_inode(dir->i_sb, &e);
		if (IS_ERR(inode))
			return ERR_CAST(inode);
	}
	WRITE_ONCE(dentry->d_time, e.gen);
	alias = d_splice_alias(inode, dentry);
	if (!IS_ERR_OR_NULL(alias))
		WRITE_ONCE(alias->d_time, e.gen);
	return alias;
}

/*
 * Revalidates a dentry from its generation alone in RCU walk. When the
 * generation moved, answers from a current listing or with one LOOKUP: the
 * same id refreshes attributes and keeps the dentry, anything else
 * invalidates it.
 */
static int ovnfs_d_revalidate(struct inode *dir, const struct qstr *name,
			      struct dentry *dentry, unsigned int flags)
{
	struct ovnfs_sb *sbi = OVNFS_SB(dentry->d_sb);
	struct inode *inode = d_inode_rcu(dentry);
	struct ovnfs_entry e;
	int err;

	ovnfs_touch(sbi);
	if (!(flags & LOOKUP_REVAL) && READ_ONCE(dentry->d_time) == ovnfs_gen(sbi) &&
	    !(inode && test_bit(OVNFS_I_STALE, &OVNFS_I(inode)->flags)))
		return 1;
	if (flags & LOOKUP_RCU)
		return -ECHILD;

	inode = d_inode(dentry);
	err = -ENODATA;
	if (!(flags & LOOKUP_REVAL) && !(inode && test_bit(OVNFS_I_STALE, &OVNFS_I(inode)->flags)))
		err = ovnfs_dir_search(dir, name, &e, OVNFS_USE_CACHED);
	if (err == -ENODATA)
		err = ovnfs_lookup_call(dir, name, &e);
	if (err == -ESTALE)
		return 0;
	if (err)
		return err;

	if (!inode) {
		if (e.found)
			return 0;
	} else {
		struct ovnfs_inode *oi = OVNFS_I(inode);

		if (!e.found || le64_to_cpu(e.attr.id) != oi->id ||
		    ((le32_to_cpu(e.attr.mode) ^ inode->i_mode) & S_IFMT))
			return 0;
		clear_bit(OVNFS_I_STALE, &oi->flags);
		if (e.trusted && READ_ONCE(oi->attr_gen) != e.gen)
			ovnfs_apply_attr(inode, &e.attr, e.gen, OVNFS_ATTR_REMOTE, e.since);
	}
	WRITE_ONCE(dentry->d_time, e.gen);
	return 1;
}

const struct dentry_operations ovnfs_dentry_ops = {
	.d_revalidate = ovnfs_d_revalidate,
};

/* One entry of a per-open listing snapshot; the name lives in names. */
struct ovnfs_snap_ent {
	struct ovnfs_attr attr;
	u32 off;
	u16 len;
};

/* A listing frozen for one open directory, so seeks and changes are harmless. */
struct ovnfs_snap {
	u64 gen;
	u64 since;
	int prime;		/* attributes are current and trusted: prime the dcache */
	u32 n;
	u32 cap;
	struct ovnfs_snap_ent *ents;
	char *names;
	u32 names_len;
	u32 names_cap;
};

/* Frees a snapshot. */
static void ovnfs_snap_free(struct ovnfs_snap *s)
{
	if (!s)
		return;
	kvfree(s->ents);
	kvfree(s->names);
	kfree(s);
}

/* ovnfs_list sink (and table copier) appending to a snapshot. */
static int ovnfs_snap_add(void *ctx, const struct ovnfs_attr *a, const char *name, u16 len)
{
	struct ovnfs_snap *s = ctx;
	struct ovnfs_snap_ent *e;

	if (s->n == s->cap) {
		u32 cap = s->cap ? s->cap * 2 : 64;
		void *p = kvrealloc(s->ents, array_size(cap, sizeof(*s->ents)), GFP_KERNEL);

		if (!p)
			return -ENOMEM;
		s->ents = p;
		s->cap = cap;
	}
	if (s->names_cap - s->names_len < len) {
		u32 cap = max(s->names_cap * 2, s->names_len + OVNFS_NAME_MAX + 1024);
		void *p = kvrealloc(s->names, cap, GFP_KERNEL);

		if (!p)
			return -ENOMEM;
		s->names = p;
		s->names_cap = cap;
	}
	e = &s->ents[s->n++];
	e->attr = *a;
	e->off = s->names_len;
	e->len = len;
	memcpy(s->names + s->names_len, name, len);
	s->names_len += len;
	return 0;
}

/* Copies a ready listing into a snapshot; dir_lock held. */
static int ovnfs_snap_copy(struct inode *dir, struct ovnfs_snap *s)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	struct rb_node *n;
	int err;

	for (n = rb_first(&di->dir_ents); n; n = rb_next(n)) {
		struct ovnfs_dent *d = rb_entry(n, struct ovnfs_dent, node);

		err = ovnfs_snap_add(s, &d->attr, d->name, d->len);
		if (err)
			return err;
	}
	s->gen = di->dir_gen;
	s->since = di->dir_since;
	s->prime = di->dir_evicts == atomic64_read(&OVNFS_SB(dir->i_sb)->evict_seq);
	return 0;
}

/*
 * Takes a snapshot of the directory: a copy of its listing (made now if
 * needed), or for a directory too big to keep listed, a READDIR sequence
 * straight into the snapshot.
 */
static struct ovnfs_snap *ovnfs_dir_snapshot(struct inode *dir)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	struct ovnfs_snap *s = kzalloc(sizeof(*s), GFP_KERNEL);
	struct ovnfs_listing l;
	int err;

	if (!s)
		return ERR_PTR(-ENOMEM);
	mutex_lock(&di->dir_lock);
	err = ovnfs_dir_ready(dir, OVNFS_USE_FILL);
	if (!err)
		err = ovnfs_snap_copy(dir, s);
	mutex_unlock(&di->dir_lock);
	if (err == -ENODATA) {
		err = ovnfs_list(dir, ovnfs_snap_add, s, &l);
		s->gen = l.gen;
		s->since = l.since;
		s->prime = 0;
	}
	if (err) {
		ovnfs_snap_free(s);
		return ERR_PTR(err);
	}
	return s;
}

/*
 * Puts one listed entry into the dcache with its inode and attributes (the
 * fuse_direntplus_link pattern): a dentry already naming the same id is
 * refreshed and revalidated, a contradicting one is invalidated and
 * replaced, a missing one is allocated in-lookup and spliced. Called with the
 * directory's i_rwsem shared and no in-lookup dentry held.
 */
static void ovnfs_prime(struct dentry *parent, const struct ovnfs_snap *s,
			const struct ovnfs_snap_ent *ent)
{
	DECLARE_WAIT_QUEUE_HEAD_ONSTACK(wq);
	struct qstr q = QSTR_INIT(s->names + ent->off, ent->len);
	u64 id = le64_to_cpu(ent->attr.id);
	struct dentry *dentry, *alias;
	struct inode *inode;
	int tries = 0;

	q.hash = full_name_hash(parent, q.name, q.len);
	dentry = d_lookup(parent, &q);
	for (;;) {
		if (!dentry) {
			dentry = d_alloc_parallel(parent, &q, &wq);
			if (IS_ERR(dentry))
				return;
		}
		if (d_in_lookup(dentry))
			break;
		inode = d_inode(dentry);
		if (inode && OVNFS_I(inode)->id == id &&
		    !((inode->i_mode ^ le32_to_cpu(ent->attr.mode)) & S_IFMT)) {
			if (READ_ONCE(OVNFS_I(inode)->attr_gen) != s->gen)
				ovnfs_apply_attr(inode, &ent->attr, s->gen, OVNFS_ATTR_REMOTE, s->since);
			WRITE_ONCE(dentry->d_time, s->gen);
			dput(dentry);
			return;
		}
		d_invalidate(dentry);
		dput(dentry);
		dentry = NULL;
		if (++tries == 2)
			return;
	}

	inode = ovnfs_iget(parent->d_sb, &ent->attr, s->gen, OVNFS_ATTR_REMOTE, s->since);
	if (IS_ERR(inode)) {
		d_lookup_done(dentry);
		dput(dentry);
		return;
	}
	WRITE_ONCE(dentry->d_time, s->gen);
	alias = d_splice_alias(inode, dentry);
	d_lookup_done(dentry);
	if (!IS_ERR_OR_NULL(alias)) {
		WRITE_ONCE(alias->d_time, s->gen);
		dput(alias);
	}
	dput(dentry);
}

/*
 * Serves getdents from the open file's snapshot, taken at position 0 (and
 * again on rewind). A fresh snapshot of a current, trusted listing first
 * primes the dcache, so the stats that usually follow cost no call.
 */
static int ovnfs_iterate(struct file *file, struct dir_context *ctx)
{
	struct inode *dir = file_inode(file);
	struct ovnfs_snap *s = file->private_data;

	ovnfs_touch(OVNFS_SB(dir->i_sb));
	if (!s || ctx->pos == 0) {
		struct ovnfs_snap *fresh = ovnfs_dir_snapshot(dir);
		u32 i;

		if (IS_ERR(fresh))
			return PTR_ERR(fresh);
		ovnfs_snap_free(s);
		s = fresh;
		file->private_data = s;
		if (s->prime && s->gen == ovnfs_gen(OVNFS_SB(dir->i_sb))) {
			for (i = 0; i < s->n; i++)
				ovnfs_prime(file->f_path.dentry, s, &s->ents[i]);
		}
	}
	if (!dir_emit_dots(file, ctx))
		return 0;
	for (; ctx->pos - 2 < s->n; ctx->pos++) {
		const struct ovnfs_snap_ent *e = &s->ents[ctx->pos - 2];

		if (!dir_emit(ctx, s->names + e->off, e->len, le64_to_cpu(e->attr.id),
			      fs_umode_to_dtype(le32_to_cpu(e->attr.mode))))
			break;
	}
	return 0;
}

/* Frees the open directory's snapshot. */
static int ovnfs_dir_release(struct inode *inode, struct file *file)
{
	ovnfs_snap_free(file->private_data);
	return 0;
}

/* Makes the directory's names durable with one FSYNC. */
static int ovnfs_dir_fsync(struct file *file, loff_t start, loff_t end, int datasync)
{
	struct ovnfs_msg m = { .op = OVNFS_OP_FSYNC };

	return ovnfs_icall(file_inode(file), &m);
}

/* Records name -> a in the directory's listing if it is current under gen. */
static void ovnfs_dir_set(struct inode *dir, const struct qstr *name,
			  const struct ovnfs_attr *a, u64 gen)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	struct ovnfs_dent *d, *old;

	mutex_lock(&di->dir_lock);
	if (di->dir_gen == gen) {
		d = ovnfs_dent_alloc(a, name->name, name->len);
		if (!d || di->dir_count == OVNFS_DIR_MAX_ENTRIES) {
			kfree(d);
			ovnfs_dir_drop_table(di);
		} else {
			old = ovnfs_dent_insert(&di->dir_ents, d);
			if (old) {
				old->attr = *a;
				kfree(d);
			} else {
				di->dir_count++;
			}
		}
	}
	mutex_unlock(&di->dir_lock);
}

/* Removes name from the directory's listing if it is current under gen. */
static void ovnfs_dir_unset(struct inode *dir, const struct qstr *name, u64 gen)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	struct ovnfs_dent *d;

	mutex_lock(&di->dir_lock);
	if (di->dir_gen == gen) {
		d = ovnfs_dent_find(di, name->name, name->len);
		if (d) {
			rb_erase(&d->node, &di->dir_ents);
			di->dir_count--;
			kfree(d);
		}
	}
	mutex_unlock(&di->dir_lock);
}

/*
 * Reacts to a failed namespace mutation: when the daemon refused it because
 * the name's state differs from what the caches said, or when no reply came
 * (the mutation may have happened), the listing and the dentry are no longer
 * trusted and are asked for again.
 */
static void ovnfs_dir_doubt(struct inode *dir, struct dentry *dentry,
			    const struct ovnfs_msg *m, int err)
{
	struct ovnfs_inode *di = OVNFS_I(dir);

	switch (err) {
	case -EEXIST:
	case -ENOENT:
	case -ENOTEMPTY:
	case -ENOTDIR:
	case -EISDIR:
		break;
	default:
		if (!m->status)
			return;
	}
	mutex_lock(&di->dir_lock);
	ovnfs_dir_drop_table(di);
	mutex_unlock(&di->dir_lock);
	WRITE_ONCE(dentry->d_time, 0);
}

/*
 * Counts a subdirectory more or less in a directory's link count. A count
 * below 2 means the daemon's host does not count subdirectories; it is left
 * alone, as is the floor of 2.
 */
static void ovnfs_dir_nlink_add(struct inode *dir, int delta)
{
	if (dir->i_nlink < 2 || (delta < 0 && dir->i_nlink == 2))
		return;
	if (delta > 0)
		inc_nlink(dir);
	else
		drop_nlink(dir);
}

/* Notes that this guest changed the directory: times now, an own change. */
static void ovnfs_dir_changed(struct inode *dir)
{
	inode_set_mtime_to_ts(dir, inode_set_ctime_current(dir));
	ovnfs_own_change(dir);
}

/* Marks a directory this guest just created as completely (emptily) listed. */
static void ovnfs_dir_init_empty(struct inode *dir, u64 gen)
{
	struct ovnfs_inode *di = OVNFS_I(dir);
	struct ovnfs_sb *sbi = OVNFS_SB(dir->i_sb);

	mutex_lock(&di->dir_lock);
	ovnfs_dir_drop_table(di);
	di->dir_since = atomic64_read(&sbi->attr_seq);
	di->dir_evicts = atomic64_read(&sbi->evict_seq);
	WRITE_ONCE(di->dir_gen, gen);
	mutex_unlock(&di->dir_lock);
}

/*
 * Turns the reply of a creating call into the new name's inode and records
 * it in the directory's listing. Returns the inode with a reference.
 */
static struct inode *ovnfs_created(struct inode *dir, struct dentry *dentry,
				   const struct ovnfs_attr *a, u64 gen, umode_t type)
{
	struct inode *inode;

	if ((le32_to_cpu(a->mode) & S_IFMT) != type)
		return ERR_PTR(-EIO);
	inode = ovnfs_iget(dir->i_sb, a, gen, OVNFS_ATTR_OWN, 0);
	if (IS_ERR(inode))
		return inode;
	ovnfs_dir_set(dir, &dentry->d_name, a, gen);
	ovnfs_dir_changed(dir);
	WRITE_ONCE(dentry->d_time, gen);
	return inode;
}

/* Creates (or with excl only creates) a regular file with one CREATE. */
static int ovnfs_create(struct mnt_idmap *idmap, struct inode *dir,
			struct dentry *dentry, umode_t mode, bool excl)
{
	struct ovnfs_create_req req = {
		.mode = cpu_to_le32(mode & S_IALLUGO),
		.flags = cpu_to_le32(excl ? OVNFS_CREATE_EXCL : 0),
	};
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_CREATE,
		.arg = &req,
		.arg_len = sizeof(req),
		.name = dentry->d_name.name,
		.name_len = dentry->d_name.len,
		.res = &a,
		.res_len = sizeof(a),
	};
	struct inode *inode;
	int err;

	err = ovnfs_icall(dir, &m);
	if (err) {
		ovnfs_dir_doubt(dir, dentry, &m, err);
		return err;
	}
	inode = ovnfs_created(dir, dentry, &a, m.gen, S_IFREG);
	if (IS_ERR(inode))
		return PTR_ERR(inode);
	d_instantiate(dentry, inode);
	return 0;
}

/* Creates a directory with one MKDIR; it starts out completely listed. */
static struct dentry *ovnfs_mkdir(struct mnt_idmap *idmap, struct inode *dir,
				  struct dentry *dentry, umode_t mode)
{
	struct ovnfs_mkdir_req req = { .mode = cpu_to_le32(mode & S_IALLUGO) };
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_MKDIR,
		.arg = &req,
		.arg_len = sizeof(req),
		.name = dentry->d_name.name,
		.name_len = dentry->d_name.len,
		.res = &a,
		.res_len = sizeof(a),
	};
	struct inode *inode;
	struct dentry *alias;
	int err;

	err = ovnfs_icall(dir, &m);
	if (err) {
		ovnfs_dir_doubt(dir, dentry, &m, err);
		return ERR_PTR(err);
	}
	inode = ovnfs_created(dir, dentry, &a, m.gen, S_IFDIR);
	if (IS_ERR(inode))
		return ERR_CAST(inode);
	ovnfs_dir_init_empty(inode, m.gen);
	ovnfs_dir_nlink_add(dir, 1);
	d_drop(dentry);
	alias = d_splice_alias(inode, dentry);
	if (!IS_ERR_OR_NULL(alias))
		WRITE_ONCE(alias->d_time, m.gen);
	return alias;
}

/* Creates a FIFO, socket or device node with one MKNOD (a regular file with CREATE). */
static int ovnfs_mknod(struct mnt_idmap *idmap, struct inode *dir,
		       struct dentry *dentry, umode_t mode, dev_t rdev)
{
	struct ovnfs_mknod_req req = {
		.mode = cpu_to_le32(mode & (S_IFMT | S_IALLUGO)),
		.rdev = cpu_to_le32(new_encode_dev(rdev)),
	};
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_MKNOD,
		.arg = &req,
		.arg_len = sizeof(req),
		.name = dentry->d_name.name,
		.name_len = dentry->d_name.len,
		.res = &a,
		.res_len = sizeof(a),
	};
	struct inode *inode;
	int err;

	if (S_ISREG(mode))
		return ovnfs_create(idmap, dir, dentry, mode, true);
	err = ovnfs_icall(dir, &m);
	if (err) {
		ovnfs_dir_doubt(dir, dentry, &m, err);
		return err;
	}
	inode = ovnfs_created(dir, dentry, &a, m.gen, mode & S_IFMT);
	if (IS_ERR(inode))
		return PTR_ERR(inode);
	d_instantiate(dentry, inode);
	return 0;
}

/* Creates a symlink with one SYMLINK; its target is cached at once. */
static int ovnfs_symlink(struct mnt_idmap *idmap, struct inode *dir,
			 struct dentry *dentry, const char *target)
{
	size_t tlen = strlen(target);
	struct ovnfs_symlink_req req = { .name_len = cpu_to_le16(dentry->d_name.len) };
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_SYMLINK,
		.arg = &req,
		.arg_len = sizeof(req),
		.name = dentry->d_name.name,
		.name_len = dentry->d_name.len,
		.name2 = target,
		.res = &a,
		.res_len = sizeof(a),
	};
	struct inode *inode;
	char *copy;
	int err;

	if (tlen >= PATH_MAX)
		return -ENAMETOOLONG;
	m.name2_len = tlen;
	err = ovnfs_icall(dir, &m);
	if (err) {
		ovnfs_dir_doubt(dir, dentry, &m, err);
		return err;
	}
	inode = ovnfs_created(dir, dentry, &a, m.gen, S_IFLNK);
	if (IS_ERR(inode))
		return PTR_ERR(inode);
	copy = kmemdup_nul(target, tlen, GFP_KERNEL);
	if (copy && !ovnfs_link_publish(inode, copy, m.gen))
		kfree(copy);
	d_instantiate(dentry, inode);
	return 0;
}

/* Gives an existing file another name with one LINK. */
static int ovnfs_link(struct dentry *old_dentry, struct inode *dir, struct dentry *dentry)
{
	struct inode *inode = d_inode(old_dentry);
	struct ovnfs_link_req req = { .dir = cpu_to_le64(OVNFS_I(dir)->id) };
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_LINK,
		.arg = &req,
		.arg_len = sizeof(req),
		.name = dentry->d_name.name,
		.name_len = dentry->d_name.len,
		.res = &a,
		.res_len = sizeof(a),
	};
	int err;

	err = ovnfs_icall(inode, &m);
	if (err) {
		ovnfs_dir_doubt(dir, dentry, &m, err);
		return err;
	}
	if (le64_to_cpu(a.id) != OVNFS_I(inode)->id)
		return -EIO;
	ovnfs_apply_attr(inode, &a, m.gen, OVNFS_ATTR_OWN, 0);
	ovnfs_dir_set(dir, &dentry->d_name, &a, m.gen);
	ovnfs_dir_changed(dir);
	WRITE_ONCE(dentry->d_time, m.gen);
	ihold(inode);
	d_instantiate(dentry, inode);
	return 0;
}

/* Removes a name with one UNLINK. */
static int ovnfs_unlink(struct inode *dir, struct dentry *dentry)
{
	struct ovnfs_msg m = {
		.op = OVNFS_OP_UNLINK,
		.name = dentry->d_name.name,
		.name_len = dentry->d_name.len,
	};
	int err;

	err = ovnfs_icall(dir, &m);
	if (err) {
		ovnfs_dir_doubt(dir, dentry, &m, err);
		return err;
	}
	ovnfs_dir_unset(dir, &dentry->d_name, m.gen);
	ovnfs_dir_changed(dir);
	ovnfs_unlinked(d_inode(dentry));
	WRITE_ONCE(dentry->d_time, m.gen);
	return 0;
}

/* Removes an empty directory with one RMDIR. */
static int ovnfs_rmdir(struct inode *dir, struct dentry *dentry)
{
	struct inode *inode = d_inode(dentry);
	struct ovnfs_inode *oi = OVNFS_I(inode);
	struct ovnfs_msg m = {
		.op = OVNFS_OP_RMDIR,
		.name = dentry->d_name.name,
		.name_len = dentry->d_name.len,
	};
	int err;

	err = ovnfs_icall(dir, &m);
	if (err) {
		ovnfs_dir_doubt(dir, dentry, &m, err);
		return err;
	}
	ovnfs_dir_unset(dir, &dentry->d_name, m.gen);
	ovnfs_dir_nlink_add(dir, -1);
	ovnfs_dir_changed(dir);
	clear_nlink(inode);
	mutex_lock(&oi->dir_lock);
	ovnfs_dir_drop_table(oi);
	mutex_unlock(&oi->dir_lock);
	WRITE_ONCE(dentry->d_time, m.gen);
	return 0;
}

/*
 * Renames with one RENAME (RENAME_NOREPLACE supported, other flags EINVAL)
 * and moves the name between the listings; the VFS moves the dentry.
 */
static int ovnfs_rename(struct mnt_idmap *idmap, struct inode *old_dir,
			struct dentry *old_dentry, struct inode *new_dir,
			struct dentry *new_dentry, unsigned int flags)
{
	struct inode *inode = d_inode(old_dentry);
	struct inode *target = d_inode(new_dentry);
	struct ovnfs_rename_req req = {
		.new_dir = cpu_to_le64(OVNFS_I(new_dir)->id),
		.flags = cpu_to_le32(flags & RENAME_NOREPLACE ? OVNFS_RENAME_NOREPLACE : 0),
		.old_len = cpu_to_le16(old_dentry->d_name.len),
		.new_len = cpu_to_le16(new_dentry->d_name.len),
	};
	struct ovnfs_msg m = {
		.op = OVNFS_OP_RENAME,
		.arg = &req,
		.arg_len = sizeof(req),
		.name = old_dentry->d_name.name,
		.name_len = old_dentry->d_name.len,
		.name2 = new_dentry->d_name.name,
		.name2_len = new_dentry->d_name.len,
	};
	struct ovnfs_attr a;
	int err;

	if (flags & ~RENAME_NOREPLACE)
		return -EINVAL;
	err = ovnfs_icall(old_dir, &m);
	if (err) {
		ovnfs_dir_doubt(old_dir, old_dentry, &m, err);
		ovnfs_dir_doubt(new_dir, new_dentry, &m, err);
		return err;
	}

	ovnfs_attr_from_inode(inode, &a);
	ovnfs_dir_unset(old_dir, &old_dentry->d_name, m.gen);
	ovnfs_dir_set(new_dir, &new_dentry->d_name, &a, m.gen);
	if (target) {
		if (S_ISDIR(target->i_mode)) {
			clear_nlink(target);
			ovnfs_dir_nlink_add(new_dir, -1);
		} else {
			ovnfs_unlinked(target);
		}
	}
	if (S_ISDIR(inode->i_mode) && old_dir != new_dir) {
		ovnfs_dir_nlink_add(old_dir, -1);
		ovnfs_dir_nlink_add(new_dir, 1);
	}
	ovnfs_dir_changed(old_dir);
	if (new_dir != old_dir)
		ovnfs_dir_changed(new_dir);
	WRITE_ONCE(old_dentry->d_time, m.gen);
	WRITE_ONCE(new_dentry->d_time, m.gen);
	return 0;
}

const struct inode_operations ovnfs_dir_iops = {
	.lookup = ovnfs_lookup,
	.create = ovnfs_create,
	.link = ovnfs_link,
	.unlink = ovnfs_unlink,
	.symlink = ovnfs_symlink,
	.mkdir = ovnfs_mkdir,
	.rmdir = ovnfs_rmdir,
	.mknod = ovnfs_mknod,
	.rename = ovnfs_rename,
	.getattr = ovnfs_getattr,
	.setattr = ovnfs_setattr,
};

const struct file_operations ovnfs_dir_fops = {
	.llseek = generic_file_llseek,
	.read = generic_read_dir,
	.iterate_shared = ovnfs_iterate,
	.release = ovnfs_dir_release,
	.fsync = ovnfs_dir_fsync,
};
