// SPDX-License-Identifier: GPL-2.0-only
/*
 * call.c - the trap transport.
 *
 * A call is one 64-bit write of a struct ovnfs_call's guest-physical address
 * to a doorbell register. The VMM traps the write on this vCPU, relays the
 * request to the daemon and resumes the guest once the reply is in memory, so
 * the call is complete when writeq() returns: no interrupt, no wait queue.
 *
 * Each port owns a physically contiguous call area holding the call with its
 * segment table, a reply scratch for the reply header and fixed result, and a
 * request scratch for the header, body and names. The kernel stack is
 * vmalloc'ed and never becomes a segment. Bulk data (page-cache folios,
 * listing buffers) is passed by physical address.
 */
#include <linux/cred.h>
#include <linux/debugfs.h>
#include <linux/errno.h>
#include <linux/gfp.h>
#include <linux/io.h>
#include <linux/mm.h>
#include <linux/module.h>
#include <linux/moduleparam.h>
#include <linux/percpu.h>
#include <linux/pfn.h>
#include <linux/seq_file.h>
#include <linux/slab.h>
#include <linux/timekeeping.h>
#include <linux/uidgid.h>
#include <linux/vmalloc.h>

#include "ovnfs.h"

#if defined(CONFIG_ARM64)
#define OVNFS_MMIO_BASE_DEFAULT	OVNFS_MMIO_BASE_ARM64
#elif defined(CONFIG_X86_64)
#define OVNFS_MMIO_BASE_DEFAULT	OVNFS_MMIO_BASE_X86_64
#else
#error "ovnfs supports arm64 and x86_64 guests"
#endif

static unsigned long base = OVNFS_MMIO_BASE_DEFAULT;
module_param(base, ulong, 0444);
MODULE_PARM_DESC(base, "guest-physical address of the ovnfs call page");

/* Layout of a port's call area. */
#define OVNFS_CALL_BYTES	(sizeof(struct ovnfs_call) + OVNFS_MAX_SEGS * sizeof(struct ovnfs_seg))
#define OVNFS_REP_OFF		ALIGN(OVNFS_CALL_BYTES, 64)
#define OVNFS_REP_BYTES		(sizeof(struct ovnfs_rep) + OVNFS_RES_MAX)
#define OVNFS_REQ_OFF		ALIGN(OVNFS_REP_OFF + OVNFS_REP_BYTES, 64)
#define OVNFS_AREA_BYTES	(OVNFS_REQ_OFF + OVNFS_REQ_MAX)

#define OVNFS_NR_OPS		(OVNFS_OP_ALLOC_IDS + 1)

/* A full READ, WRITE or READDIR: request, reply header and max_io of pages. */
static_assert(OVNFS_MAX_FOLIOS + 2 <= OVNFS_MAX_SEGS);

/* One doorbell and the call area used with it. */
struct ovnfs_port {
	spinlock_t lock;	/* CPUs sharing a port when there are fewer ports than CPUs */
	void *area;
	phys_addr_t pa;
} ____cacheline_aligned_in_smp;

/* Per-CPU call statistics, exported through debugfs. */
struct ovnfs_stats {
	u64 calls[OVNFS_NR_OPS];
	u64 ns[OVNFS_NR_OPS];
	u64 counters[OVNFS_C_NR];	/* enum ovnfs_counter */
};

static void __iomem *ovnfs_mmio;
static struct ovnfs_port *ovnfs_ports;
static unsigned int ovnfs_nports;
static unsigned int ovnfs_area_order;
static struct dentry *ovnfs_debugfs;
static DEFINE_PER_CPU(struct ovnfs_stats, ovnfs_stats);

static const char *const ovnfs_op_names[OVNFS_NR_OPS] = {
	[OVNFS_OP_HELLO] = "hello",
	[OVNFS_OP_POLL] = "poll",
	[OVNFS_OP_STATFS] = "statfs",
	[OVNFS_OP_GETATTR] = "getattr",
	[OVNFS_OP_SETATTR] = "setattr",
	[OVNFS_OP_LOOKUP] = "lookup",
	[OVNFS_OP_READDIR] = "readdir",
	[OVNFS_OP_READLINK] = "readlink",
	[OVNFS_OP_READ] = "read",
	[OVNFS_OP_WRITE] = "write",
	[OVNFS_OP_FSYNC] = "fsync",
	[OVNFS_OP_CREATE] = "create",
	[OVNFS_OP_MKDIR] = "mkdir",
	[OVNFS_OP_MKNOD] = "mknod",
	[OVNFS_OP_SYMLINK] = "symlink",
	[OVNFS_OP_LINK] = "link",
	[OVNFS_OP_UNLINK] = "unlink",
	[OVNFS_OP_RMDIR] = "rmdir",
	[OVNFS_OP_RENAME] = "rename",
	[OVNFS_OP_COMPOUND] = "compound",
	[OVNFS_OP_ALLOC_IDS] = "alloc_ids",
};

static const char *const ovnfs_counter_names[OVNFS_C_NR] = {
	[OVNFS_C_BATCHED] = "batched",
	[OVNFS_C_JOURNAL + OVNFS_J_CREATE] = "journal_create",
	[OVNFS_C_JOURNAL + OVNFS_J_MKDIR] = "journal_mkdir",
	[OVNFS_C_JOURNAL + OVNFS_J_UNLINK] = "journal_unlink",
	[OVNFS_C_JOURNAL + OVNFS_J_RENAME] = "journal_rename",
	[OVNFS_C_JOURNAL + OVNFS_J_LINK] = "journal_link",
	[OVNFS_C_JOURNAL + OVNFS_J_RMDIR] = "journal_rmdir",
	[OVNFS_C_JOURNAL + OVNFS_J_SETATTR] = "journal_setattr",
	[OVNFS_C_JOURNAL + OVNFS_J_DATA] = "journal_data",
	[OVNFS_C_CREATE_DATA] = "create_data",
	[OVNFS_C_ID_REFILLS] = "id_refills",
	[OVNFS_C_LATE_FAILURES] = "late_failures",
};

/* Most entries any journal held at once. */
static atomic_t ovnfs_journal_high;

/* Segment table under construction; merges physically adjacent pieces. */
struct ovnfs_segs {
	struct ovnfs_seg *seg;
	unsigned int n;
	unsigned int first;	/* first segment of the current direction */
	u32 bytes;		/* bytes in the current direction */
};

/* Appends one physically contiguous piece; false when the table is full. */
static bool ovnfs_seg_add(struct ovnfs_segs *s, phys_addr_t pa, u32 len)
{
	struct ovnfs_seg *prev = s->n > s->first ? &s->seg[s->n - 1] : NULL;

	if (!len)
		return true;
	s->bytes += len;
	if (prev && le64_to_cpu(prev->gpa) + le32_to_cpu(prev->len) == pa &&
	    le32_to_cpu(prev->len) <= U32_MAX - len) {
		prev->len = cpu_to_le32(le32_to_cpu(prev->len) + len);
		return true;
	}
	if (s->n == OVNFS_MAX_SEGS)
		return false;
	s->seg[s->n].gpa = cpu_to_le64(pa);
	s->seg[s->n].len = cpu_to_le32(len);
	s->seg[s->n].pad = 0;
	s->n++;
	return true;
}

/* Appends a kmalloc'ed or vmalloc'ed buffer page by page. */
static bool ovnfs_seg_add_buf(struct ovnfs_segs *s, const void *p, u32 len)
{
	while (len) {
		u32 off = offset_in_page(p);
		u32 chunk = min_t(u32, len, PAGE_SIZE - off);
		phys_addr_t pa = is_vmalloc_addr(p) ?
			page_to_phys(vmalloc_to_page(p)) + off : virt_to_phys(p);

		if (!ovnfs_seg_add(s, pa, chunk))
			return false;
		p += chunk;
		len -= chunk;
	}
	return true;
}

/* Appends the first len bytes of a run of page-cache folios. */
static bool ovnfs_seg_add_folios(struct ovnfs_segs *s, struct folio **folios,
				 unsigned int nr, u32 len)
{
	unsigned int i;

	for (i = 0; i < nr && len; i++) {
		u32 chunk = min_t(size_t, len, folio_size(folios[i]));

		if (!ovnfs_seg_add(s, PFN_PHYS(folio_pfn(folios[i])), chunk))
			return false;
		len -= chunk;
	}
	return true;
}

/* Lays out the request, reply scratch and bulk segments of m in a call area. */
static bool ovnfs_build(void *area, const struct ovnfs_msg *m, u32 req_len, u32 rep_len)
{
	struct ovnfs_call *call = area;
	struct ovnfs_req *req = area + OVNFS_REQ_OFF;
	struct ovnfs_segs s = { .seg = call->seg };
	void *p = req + 1;
	unsigned int n_out;

	req->op = cpu_to_le16(m->op);
	req->pad = 0;
	req->uid = cpu_to_le32(from_kuid_munged(&init_user_ns, current_fsuid()));
	req->gid = cpu_to_le32(from_kgid_munged(&init_user_ns, current_fsgid()));
	req->pad2 = 0;
	req->id = cpu_to_le64(m->id);
	if (m->arg_len)
		memcpy(p, m->arg, m->arg_len);
	p += m->arg_len;
	if (m->name_len)
		memcpy(p, m->name, m->name_len);
	p += m->name_len;
	if (m->name2_len)
		memcpy(p, m->name2, m->name2_len);

	if (!ovnfs_seg_add(&s, virt_to_phys(req), req_len))
		return false;
	if (m->obuf && !ovnfs_seg_add_buf(&s, m->obuf, m->obuf_len))
		return false;
	if (m->op == OVNFS_OP_WRITE &&
	    !ovnfs_seg_add_folios(&s, m->folios, m->nr_folios, m->data_len))
		return false;
	n_out = s.n;

	s.first = s.n;
	s.bytes = 0;
	if (!ovnfs_seg_add(&s, virt_to_phys(area + OVNFS_REP_OFF), rep_len))
		return false;
	if (m->op != OVNFS_OP_WRITE &&
	    !ovnfs_seg_add_folios(&s, m->folios, m->nr_folios, m->data_len))
		return false;
	if (m->buf && !ovnfs_seg_add_buf(&s, m->buf, m->buf_len))
		return false;

	call->status = 0;
	call->reply_len = 0;
	call->n_out = cpu_to_le16(n_out);
	call->n_in = cpu_to_le16(s.n - n_out);
	call->pad = 0;
	return true;
}

/*
 * Performs one call on the current CPU's port and maps the outcome: a
 * nonzero transport status, a short or oversized reply and a daemon error all
 * become negative errnos. Every delivered reply's epoch (a compound's
 * excepted) is fed to the cache layer, errors included. Sleeps in the sense that it may take milliseconds
 * with preemption disabled; never call it from atomic or RCU-walk context.
 */
int ovnfs_call(struct ovnfs_sb *sbi, struct ovnfs_msg *m)
{
	u32 req_len = sizeof(struct ovnfs_req) + m->arg_len + m->name_len + m->name2_len;
	u32 rep_len = sizeof(struct ovnfs_rep) + m->res_len;
	u32 in_cap, status, reply_len;
	struct ovnfs_port *port;
	struct ovnfs_call *call;
	struct ovnfs_stats *st;
	struct ovnfs_rep rep;
	unsigned int idx;
	u64 t0;

	might_sleep();
	m->status = 0;
	m->data_got = 0;
	if (req_len > OVNFS_REQ_MAX || m->res_len > OVNFS_RES_MAX ||
	    m->obuf_len > OVNFS_MAX_FRAME - req_len)
		return -EINVAL;
	if (m->op != OVNFS_OP_POLL)
		ovnfs_touch(sbi);
	in_cap = rep_len + (m->op == OVNFS_OP_WRITE ? 0 : m->data_len) + (m->buf ? m->buf_len : 0);
	if (in_cap > OVNFS_MAX_FRAME)
		return -EINVAL;

	preempt_disable();
	idx = smp_processor_id() % ovnfs_nports;
	port = &ovnfs_ports[idx];
	spin_lock(&port->lock);
	call = port->area;
	if (!ovnfs_build(port->area, m, req_len, rep_len)) {
		spin_unlock(&port->lock);
		preempt_enable();
		return -EINVAL;
	}
	t0 = ktime_get_ns();
	/* writeq orders the call area's stores before the doorbell. */
	writeq(port->pa, ovnfs_mmio + OVNFS_REG_DOORBELL + 8 * idx);
	/* The VMM filled the reply before resuming us; read it after the trap. */
	rmb();
	st = this_cpu_ptr(&ovnfs_stats);
	st->calls[m->op]++;
	st->ns[m->op] += ktime_get_ns() - t0;
	status = le32_to_cpu(READ_ONCE(call->status));
	reply_len = le32_to_cpu(READ_ONCE(call->reply_len));
	memcpy(&rep, port->area + OVNFS_REP_OFF, sizeof(rep));
	if (!status && m->res_len && reply_len > sizeof(rep))
		memcpy(m->res, port->area + OVNFS_REP_OFF + sizeof(rep),
		       min_t(u32, m->res_len, reply_len - sizeof(rep)));
	spin_unlock(&port->lock);
	preempt_enable();

	if (status) {
		m->status = status;
		return ovnfs_errno(status);
	}
	if (reply_len < sizeof(rep) || reply_len > in_cap)
		return -EIO;
	/*
	 * A compound's sub-replies carry their own epochs, fed in order by the
	 * caller; feeding the outer one too could interleave an older epoch
	 * behind a newer one and bump the generation for nothing.
	 */
	if (m->op != OVNFS_OP_COMPOUND)
		m->gen = ovnfs_note_epoch(sbi, le64_to_cpu(rep.epoch));
	if (rep.error)
		return ovnfs_errno(le32_to_cpu(rep.error));
	if (reply_len < rep_len)
		return -EIO;
	m->data_got = reply_len - rep_len;
	return 0;
}

/* Adds n to an event counter. */
void ovnfs_count(enum ovnfs_counter c, unsigned int n)
{
	this_cpu_add(ovnfs_stats.counters[c], n);
}

/* Records a journal length for the high-water mark. */
void ovnfs_note_journal_high(u32 entries)
{
	int high = atomic_read(&ovnfs_journal_high);

	while ((int)entries > high && !atomic_try_cmpxchg(&ovnfs_journal_high, &high, entries))
		;
}

/*
 * Prints one line per op: name, calls, total nanoseconds in calls; then one
 * line per event counter and the journal's high-water mark, as
 * "<name> <count> 0".
 */
static int ovnfs_calls_show(struct seq_file *s, void *unused)
{
	unsigned int op, c;
	int cpu;

	for (op = 1; op < OVNFS_NR_OPS; op++) {
		u64 calls = 0, ns = 0;

		for_each_possible_cpu(cpu) {
			const struct ovnfs_stats *st = per_cpu_ptr(&ovnfs_stats, cpu);

			calls += READ_ONCE(st->calls[op]);
			ns += READ_ONCE(st->ns[op]);
		}
		seq_printf(s, "%s %llu %llu\n", ovnfs_op_names[op], calls, ns);
	}
	for (c = 0; c < OVNFS_C_NR; c++) {
		u64 n = 0;

		for_each_possible_cpu(cpu)
			n += READ_ONCE(per_cpu_ptr(&ovnfs_stats, cpu)->counters[c]);
		seq_printf(s, "%s %llu 0\n", ovnfs_counter_names[c], n);
	}
	seq_printf(s, "journal_high %d 0\n", atomic_read(&ovnfs_journal_high));
	return 0;
}
DEFINE_SHOW_ATTRIBUTE(ovnfs_calls);

/* Frees the call areas and unmaps the call page. */
void ovnfs_transport_exit(void)
{
	unsigned int i;

	debugfs_remove(ovnfs_debugfs);
	ovnfs_debugfs = NULL;
	if (ovnfs_ports) {
		for (i = 0; i < ovnfs_nports; i++)
			free_pages((unsigned long)ovnfs_ports[i].area, ovnfs_area_order);
		kfree(ovnfs_ports);
		ovnfs_ports = NULL;
	}
	if (ovnfs_mmio) {
		iounmap(ovnfs_mmio);
		ovnfs_mmio = NULL;
	}
}

/* Maps the call page, checks INFO and allocates one call area per port. */
int ovnfs_transport_init(void)
{
	unsigned int i, version, ports;
	u64 info;

	ovnfs_mmio = ioremap(base, OVNFS_MMIO_SIZE);
	if (!ovnfs_mmio)
		return -ENOMEM;
	info = readq(ovnfs_mmio + OVNFS_REG_INFO);
	version = (info >> 32) & 0xffff;
	ports = info >> 48;
	if (lower_32_bits(info) != OVNFS_CALL_MAGIC || version != OVNFS_CALL_VERSION ||
	    !ports || ports > OVNFS_MAX_PORTS) {
		pr_err("ovnfs: no call device at %#lx (info %#llx)\n", base, info);
		ovnfs_transport_exit();
		return -ENODEV;
	}

	ovnfs_area_order = get_order(OVNFS_AREA_BYTES);
	ovnfs_ports = kcalloc(ports, sizeof(*ovnfs_ports), GFP_KERNEL);
	if (!ovnfs_ports) {
		ovnfs_transport_exit();
		return -ENOMEM;
	}
	ovnfs_nports = ports;
	for (i = 0; i < ports; i++) {
		struct ovnfs_port *port = &ovnfs_ports[i];

		spin_lock_init(&port->lock);
		port->area = (void *)__get_free_pages(GFP_KERNEL | __GFP_ZERO, ovnfs_area_order);
		if (!port->area) {
			ovnfs_transport_exit();
			return -ENOMEM;
		}
		port->pa = virt_to_phys(port->area);
	}

	ovnfs_debugfs = debugfs_create_dir("ovnfs", NULL);
	debugfs_create_file("calls", 0444, ovnfs_debugfs, NULL, &ovnfs_calls_fops);
	pr_info("ovnfs: call device at %#lx, %u ports\n", base, ports);
	return 0;
}
