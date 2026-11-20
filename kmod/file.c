// SPDX-License-Identifier: GPL-2.0-only
/*
 * file.c - regular files: page-cache I/O with order-0 folios.
 *
 * Reads fill folios straight from the daemon (READ, one per readahead
 * window up to max_io). Writes dirty the page cache; writeback batches
 * contiguous dirty folios into WRITE calls. Closing a file writes its dirty
 * pages back, so the host sees a closed file's data without any sync on the
 * daemon; fsync adds FSYNC. Folios are passed to the daemon by physical
 * address and stay locked while a call uses them.
 */
#include <linux/fs.h>
#include <linux/highmem.h>
#include <linux/mm.h>
#include <linux/pagemap.h>
#include <linux/slab.h>
#include <linux/uio.h>
#include <linux/writeback.h>

#include "ovnfs.h"

/*
 * Reads n consecutive locked folios with one READ; *got is the byte count
 * the daemon returned (short only at end of file). An orphan has no daemon
 * copy: whatever is not cached is gone.
 */
static int ovnfs_read_folios(struct inode *inode, struct folio **folios, unsigned int n, u32 *got)
{
	u32 len = n * PAGE_SIZE;
	struct ovnfs_read_req req = {
		.off = cpu_to_le64(folio_pos(folios[0])),
		.len = cpu_to_le32(len),
	};
	struct ovnfs_msg m = {
		.op = OVNFS_OP_READ,
		.arg = &req,
		.arg_len = sizeof(req),
		.folios = folios,
		.nr_folios = n,
		.data_len = len,
	};
	int err;

	*got = 0;
	if (test_bit(OVNFS_I_ORPHAN, &OVNFS_I(inode)->flags))
		return -ESTALE;
	err = ovnfs_icall(inode, &m);
	if (!err)
		*got = m.data_got;
	return err;
}

/* Zeroes what a READ did not fill (beyond end of file) and flushes caches. */
static void ovnfs_read_done(struct folio *folio, u32 filled)
{
	if (filled < folio_size(folio))
		folio_zero_segment(folio, filled, folio_size(folio));
	flush_dcache_folio(folio);
}

/* Fills one locked folio and marks it uptodate; leaves it locked. */
static int ovnfs_fill_folio(struct inode *inode, struct folio *folio)
{
	u32 got;
	int err;

	err = ovnfs_read_folios(inode, &folio, 1, &got);
	if (err)
		return err;
	ovnfs_read_done(folio, got);
	folio_mark_uptodate(folio);
	return 0;
}

/* read_folio: one READ for one folio. */
static int ovnfs_read_folio(struct file *file, struct folio *folio)
{
	int err = ovnfs_fill_folio(folio->mapping->host, folio);

	folio_unlock(folio);
	return err;
}

/* readahead: one READ per window, split only at max_io. */
static void ovnfs_readahead(struct readahead_control *ractl)
{
	struct inode *inode = ractl->mapping->host;
	unsigned int cap = min_t(unsigned int, readahead_count(ractl),
				 OVNFS_SB(inode->i_sb)->max_io >> PAGE_SHIFT);
	struct folio *one, **batch = NULL;
	unsigned int n, i;
	u32 got;
	int err;

	if (cap > 1)
		batch = kmalloc_array(cap, sizeof(*batch), GFP_NOFS | __GFP_NOWARN);
	if (!batch) {
		batch = &one;
		cap = 1;
	}
	for (;;) {
		struct folio *folio;

		for (n = 0; n < cap && (folio = readahead_folio(ractl)); n++)
			batch[n] = folio;
		if (!n)
			break;
		err = ovnfs_read_folios(inode, batch, n, &got);
		for (i = 0; i < n; i++) {
			u32 off = i * PAGE_SIZE;

			if (!err)
				ovnfs_read_done(batch[i], got > off ? min_t(u32, got - off, PAGE_SIZE) : 0);
			folio_end_read(batch[i], !err);
		}
	}
	if (batch != &one)
		kfree(batch);
}

/*
 * Prepares a folio for a buffered write: a partial write into a folio that
 * is not cached reads it first, unless it lies wholly beyond end of file.
 */
static int ovnfs_write_begin(struct file *file, struct address_space *mapping,
			     loff_t pos, unsigned int len, struct folio **foliop, void **fsdata)
{
	struct inode *inode = mapping->host;
	struct folio *folio;
	size_t off;
	int err;

	folio = __filemap_get_folio(mapping, pos >> PAGE_SHIFT, FGP_WRITEBEGIN,
				    mapping_gfp_mask(mapping));
	if (IS_ERR(folio))
		return PTR_ERR(folio);
	*foliop = folio;
	if (folio_test_uptodate(folio))
		return 0;
	off = offset_in_folio(folio, pos);
	if (off == 0 && len == folio_size(folio))
		return 0;
	if (folio_pos(folio) >= i_size_read(inode)) {
		folio_zero_segments(folio, 0, off, off + len, folio_size(folio));
		return 0;
	}
	err = ovnfs_fill_folio(inode, folio);
	if (err) {
		folio_unlock(folio);
		folio_put(folio);
	}
	return err;
}

/*
 * Commits a buffered write: dirties the folio first, then extends i_size
 * under i_lock, so attribute refreshes (which check the dirty tag under the
 * same lock) never shrink a size this guest just produced.
 */
static int ovnfs_write_end(struct file *file, struct address_space *mapping,
			   loff_t pos, unsigned int len, unsigned int copied,
			   struct folio *folio, void *fsdata)
{
	struct inode *inode = mapping->host;
	size_t off = offset_in_folio(folio, pos);

	if (!folio_test_uptodate(folio)) {
		if (copied < len) {
			if (off == 0 && len == folio_size(folio)) {
				copied = 0;
				goto out;
			}
			folio_zero_range(folio, off + copied, len - copied);
		}
		folio_mark_uptodate(folio);
	}
	if (copied) {
		folio_mark_dirty(folio);
		spin_lock(&inode->i_lock);
		if (pos + copied > i_size_read(inode))
			i_size_write(inode, pos + copied);
		spin_unlock(&inode->i_lock);
	}
out:
	folio_unlock(folio);
	folio_put(folio);
	return copied;
}

/* Contiguous dirty folios collected for one WRITE; all locked, under writeback. */
struct ovnfs_wb {
	struct inode *inode;
	struct folio **folios;
	unsigned int n;
	unsigned int cap;
};

/*
 * Sends the batch as one WRITE (nothing for an orphan, whose pages are its
 * only store) and ends writeback. A transport failure redirties the folios,
 * since the data may not have arrived; a daemon error is recorded on the
 * mapping for fsync and close to report.
 */
static int ovnfs_wb_flush(struct ovnfs_wb *wb, struct writeback_control *wbc)
{
	struct inode *inode = wb->inode;
	struct ovnfs_inode *oi = OVNFS_I(inode);
	loff_t pos = folio_pos(wb->folios[0]);
	loff_t len = min_t(loff_t, i_size_read(inode) - pos, (loff_t)wb->n * PAGE_SIZE);
	struct ovnfs_write_req req = {
		.off = cpu_to_le64(pos),
	};
	struct ovnfs_attr a;
	struct ovnfs_msg m = {
		.op = OVNFS_OP_WRITE,
		.arg = &req,
		.arg_len = sizeof(req),
		.folios = wb->folios,
		.nr_folios = wb->n,
		.res = &a,
		.res_len = sizeof(a),
	};
	unsigned int i;
	int err = 0;

	if (len > 0 && !test_bit(OVNFS_I_ORPHAN, &oi->flags)) {
		req.len = cpu_to_le32(len);
		m.data_len = len;
		err = ovnfs_icall(inode, &m);
		if (!err && le64_to_cpu(a.id) == oi->id)
			ovnfs_apply_attr(inode, &a, m.gen, OVNFS_ATTR_OWN, 0);
		else if (err && test_bit(OVNFS_I_ORPHAN, &oi->flags))
			err = 0;
	}
	for (i = 0; i < wb->n; i++) {
		struct folio *folio = wb->folios[i];

		if (err && m.status)
			folio_redirty_for_writepage(wbc, folio);
		folio_end_writeback(folio);
		folio_unlock(folio);
	}
	if (err && !m.status)
		mapping_set_error(inode->i_mapping, err);
	wb->n = 0;
	return err;
}

/*
 * Takes one folio from writeback_iter: flushes the batch when the folio does
 * not continue it, then starts writeback and appends it. A folio wholly
 * beyond end of file (racing truncate) is cleaned without I/O.
 */
static int ovnfs_wb_add(struct ovnfs_wb *wb, struct writeback_control *wbc, struct folio *folio)
{
	int err = 0;

	if (wb->n && (wb->n == wb->cap || folio->index != wb->folios[wb->n - 1]->index + 1))
		err = ovnfs_wb_flush(wb, wbc);
	folio_start_writeback(folio);
	if (folio_pos(folio) >= i_size_read(wb->inode)) {
		folio_end_writeback(folio);
		folio_unlock(folio);
		return err;
	}
	wb->folios[wb->n++] = folio;
	return err;
}

/* writepages: contiguous dirty folios go out in WRITE calls of up to max_io. */
static int ovnfs_writepages(struct address_space *mapping, struct writeback_control *wbc)
{
	struct inode *inode = mapping->host;
	unsigned int cap = OVNFS_SB(inode->i_sb)->max_io >> PAGE_SHIFT;
	struct folio *one, *folio = NULL;
	struct ovnfs_wb wb = { .inode = inode };
	int error = 0, err = 0;

	wb.folios = kmalloc_array(cap, sizeof(*wb.folios), GFP_NOFS | __GFP_NOWARN);
	wb.cap = cap;
	if (!wb.folios) {
		wb.folios = &one;
		wb.cap = 1;
	}
	while ((folio = writeback_iter(mapping, wbc, folio, &error)))
		error = ovnfs_wb_add(&wb, wbc, folio);
	if (wb.n)
		err = ovnfs_wb_flush(&wb, wbc);
	if (wb.folios != &one)
		kfree(wb.folios);
	return error ?: err;
}

/*
 * Opens a file, refreshing stale attributes first (which drops the page
 * cache when the file changed elsewhere), and counts the open: an unlinked
 * file stays readable from its pages while open.
 */
static int ovnfs_open(struct inode *inode, struct file *file)
{
	int err;

	ovnfs_touch(OVNFS_SB(inode->i_sb));
	err = generic_file_open(inode, file);
	if (err)
		return err;
	err = ovnfs_revalidate_attr(inode);
	if (err)
		return err;
	atomic_inc(&OVNFS_I(inode)->opens);
	return 0;
}

/* Drops the open count. */
static int ovnfs_release(struct inode *inode, struct file *file)
{
	atomic_dec(&OVNFS_I(inode)->opens);
	return 0;
}

/* On close, writes dirty pages back synchronously and reports write errors. */
static int ovnfs_flush(struct file *file, fl_owner_t id)
{
	if (!(file->f_mode & FMODE_WRITE))
		return 0;
	return file_write_and_wait(file);
}

/* Writes the range back, then makes it durable with one FSYNC. */
static int ovnfs_fsync(struct file *file, loff_t start, loff_t end, int datasync)
{
	struct inode *inode = file_inode(file);
	struct ovnfs_msg m = { .op = OVNFS_OP_FSYNC };
	int err;

	err = file_write_and_wait_range(file, start, end);
	if (err)
		return err;
	if (test_bit(OVNFS_I_ORPHAN, &OVNFS_I(inode)->flags))
		return 0;
	return ovnfs_icall(inode, &m);
}

/* Reads through the page cache after revalidating stale attributes. */
static ssize_t ovnfs_read_iter(struct kiocb *iocb, struct iov_iter *to)
{
	struct inode *inode = file_inode(iocb->ki_filp);
	int err;

	ovnfs_touch(OVNFS_SB(inode->i_sb));
	err = ovnfs_revalidate_attr(inode);
	if (err)
		return err;
	return generic_file_read_iter(iocb, to);
}

/* Writes into the page cache after revalidating stale attributes (O_APPEND needs the size). */
static ssize_t ovnfs_write_iter(struct kiocb *iocb, struct iov_iter *from)
{
	struct inode *inode = file_inode(iocb->ki_filp);
	int err;

	ovnfs_touch(OVNFS_SB(inode->i_sb));
	err = ovnfs_revalidate_attr(inode);
	if (err)
		return err;
	return generic_file_write_iter(iocb, from);
}

/* Seeks; SEEK_END and the data/hole seeks need a current size. */
static loff_t ovnfs_llseek(struct file *file, loff_t offset, int whence)
{
	if (whence != SEEK_SET && whence != SEEK_CUR) {
		int err = ovnfs_revalidate_attr(file_inode(file));

		if (err)
			return err;
	}
	return generic_file_llseek(file, offset, whence);
}

const struct file_operations ovnfs_file_fops = {
	.llseek = ovnfs_llseek,
	.read_iter = ovnfs_read_iter,
	.write_iter = ovnfs_write_iter,
	.mmap = generic_file_mmap,
	.open = ovnfs_open,
	.flush = ovnfs_flush,
	.release = ovnfs_release,
	.fsync = ovnfs_fsync,
	.splice_read = filemap_splice_read,
	.splice_write = iter_file_splice_write,
};

const struct address_space_operations ovnfs_aops = {
	.read_folio = ovnfs_read_folio,
	.readahead = ovnfs_readahead,
	.writepages = ovnfs_writepages,
	.dirty_folio = filemap_dirty_folio,
	.write_begin = ovnfs_write_begin,
	.write_end = ovnfs_write_end,
	.migrate_folio = filemap_migrate_folio,
};
