/* SPDX-License-Identifier: (GPL-2.0-only OR MIT) */
/*
 * ovnfs_wire.h - call ABI and wire protocol of the ovnfs kernel client.
 *
 * A guest reaches its checkout by trapping into the VMM, which relays one
 * request to the ovnfs daemon and resumes the guest when the reply is in guest
 * memory. Three parties share this file's definitions:
 *
 *   guest module (kmod/)        builds requests, issues calls, parses replies
 *   VMM (vibevmm devices/ovnfs) implements the call ABI; requests and replies
 *                               are opaque bytes to it
 *   daemon (kfs/)               decodes requests, encodes replies
 *
 * This header is the source of truth; the Go and Rust sides mirror it. All
 * integers are little-endian. Structs are naturally aligned, padding is
 * explicit and zero.
 */
#ifndef OVNFS_WIRE_H
#define OVNFS_WIRE_H

#include <linux/types.h>

/*
 * Call ABI (guest <-> VMM)
 *
 * The VMM maps one 4 KiB MMIO page. Its guest-physical base is fixed per
 * architecture (OVNFS_MMIO_BASE_*).
 *
 * OVNFS_REG_INFO, read as 64 bits, identifies the device:
 *   bits  0..31  OVNFS_CALL_MAGIC
 *   bits 32..47  OVNFS_CALL_VERSION
 *   bits 48..63  number of ports (at least 1)
 * A VMM without a daemon bound does not map the page at all.
 *
 * A port is one doorbell register, OVNFS_REG_DOORBELL + 8 * port. Writing the
 * guest-physical address of a struct ovnfs_call to it, as one 64-bit write,
 * performs the call: the write completes only after the VMM has sent the
 * request to the daemon, received the reply into the call's reply segments and
 * filled in status and reply_len. Ports are independent channels, each with
 * its own daemon connection; calls on one port are serialized by the VMM, so a
 * guest that uses one port per CPU never waits for another CPU's call.
 *
 * The call and every segment must lie in guest RAM. A segment may span pages
 * only where the pages are physically contiguous.
 */
#define OVNFS_MMIO_BASE_ARM64	0x09100000ULL
#define OVNFS_MMIO_BASE_X86_64	0xefff0000ULL
#define OVNFS_MMIO_SIZE		0x1000

#define OVNFS_CALL_MAGIC	0x53464e56U	/* "VNFS" */
#define OVNFS_CALL_VERSION	1

#define OVNFS_REG_INFO		0x000
#define OVNFS_REG_DOORBELL	0x100
#define OVNFS_MAX_PORTS		((OVNFS_MMIO_SIZE - OVNFS_REG_DOORBELL) / 8)

/* Largest read or write payload of one call. */
#define OVNFS_MAX_IO		(1U << 20)
/* Largest request or reply, headers included. */
#define OVNFS_MAX_FRAME		(OVNFS_MAX_IO + 4096)
/* Most segments of one call: a header buffer per direction plus 256 pages. */
#define OVNFS_MAX_SEGS		260

/* One piece of a request or reply buffer in guest memory. */
struct ovnfs_seg {
	__le64 gpa;
	__le32 len;
	__le32 pad;
};

/*
 * A call. The request is the concatenation of the first n_out segments, the
 * reply is written across the following n_in segments in order.
 *
 * status is 0 when a reply was delivered, else a positive Linux errno:
 * ENOTCONN or EIO when the daemon could not be reached or timed out, EINVAL
 * for a malformed call, EMSGSIZE when the reply did not fit the reply
 * segments. A nonzero status says nothing about whether the daemon executed
 * the request.
 */
struct ovnfs_call {
	__le32 status;		/* out */
	__le32 reply_len;	/* out: reply bytes written */
	__le16 n_out;
	__le16 n_in;
	__le32 pad;
	struct ovnfs_seg seg[];
};

/*
 * Framing (VMM <-> daemon)
 *
 * One stream connection per port. Each request and each reply is a __le32
 * byte count followed by that many bytes (at most OVNFS_MAX_FRAME), strictly
 * alternating. The VMM reconnects on its own, so no request may depend on
 * what an earlier request on the same connection did: the protocol below has
 * no per-connection state, no open files and no handles besides file ids.
 */

/*
 * Protocol (guest <-> daemon)
 *
 * A request is struct ovnfs_req followed by the op's body; a reply is struct
 * ovnfs_rep followed, when error is 0, by the op's result.
 *
 * File ids name files, not paths: an id follows its file through renames and
 * stays valid until the file is removed. OVNFS_ROOT_ID always names the root
 * of the tree. An id that names no file is answered with ESTALE.
 *
 * Names are raw bytes without NUL or '/', at most OVNFS_NAME_MAX long, never
 * "." or "..".
 */
#define OVNFS_PROTO_VERSION	1
#define OVNFS_ROOT_ID		1ULL
#define OVNFS_NAME_MAX		255

enum ovnfs_op {
	OVNFS_OP_HELLO		= 1,
	OVNFS_OP_POLL		= 2,
	OVNFS_OP_STATFS		= 3,
	OVNFS_OP_GETATTR	= 4,
	OVNFS_OP_SETATTR	= 5,
	OVNFS_OP_LOOKUP		= 6,
	OVNFS_OP_READDIR	= 7,
	OVNFS_OP_READLINK	= 8,
	OVNFS_OP_READ		= 9,
	OVNFS_OP_WRITE		= 10,
	OVNFS_OP_FSYNC		= 11,
	OVNFS_OP_CREATE		= 12,
	OVNFS_OP_MKDIR		= 13,
	OVNFS_OP_MKNOD		= 14,
	OVNFS_OP_SYMLINK	= 15,
	OVNFS_OP_LINK		= 16,
	OVNFS_OP_UNLINK		= 17,
	OVNFS_OP_RMDIR		= 18,
	OVNFS_OP_RENAME		= 19,
	OVNFS_OP_COMPOUND	= 20,
	OVNFS_OP_ALLOC_IDS	= 21,
};

/*
 * Request header. id is the file the op addresses; for ops that take a name
 * it is the directory. uid and gid are the caller's, as the guest sees them;
 * the daemon gives them to nodes the request creates.
 */
struct ovnfs_req {
	__le16 op;
	__le16 pad;
	__le32 uid;
	__le32 gid;
	__le32 pad2;
	__le64 id;
};

/*
 * Reply header. error is 0 or a positive errno in Linux numbering, whatever
 * the daemon's host is.
 *
 * epoch is the tree's cache generation, compared for equality only. It
 * changes whenever something other than this guest's own requests changed
 * the tree, and when the daemon restarts or the VM is attached to another
 * tree. Everything the guest cached under one epoch (names, missing names,
 * attributes, file contents) stays valid while replies carry that epoch.
 */
struct ovnfs_rep {
	__le32 error;
	__le32 pad;
	__le64 epoch;
};

/*
 * File attributes. mode is the Linux encoding (S_IFMT type bits plus
 * permission, setuid, setgid and sticky bits). blocks counts 512-byte units.
 * rdev is Linux's new_encode_dev() form for device nodes, else 0.
 */
struct ovnfs_attr {
	__le64 id;
	__le64 size;
	__le64 blocks;
	__le64 mtime_ns;	/* nanoseconds since the Unix epoch */
	__le64 ctime_ns;
	__le32 mode;
	__le32 nlink;
	__le32 uid;
	__le32 gid;
	__le32 rdev;
	__le32 pad;
};

/* HELLO: id is ignored. Callable at any time; binds nothing. */
struct ovnfs_hello_req {
	__le32 version;		/* OVNFS_PROTO_VERSION */
	__le32 pad;
};

#define OVNFS_HELLO_RDONLY	(1U << 0)	/* the tree refuses mutations with EROFS */

struct ovnfs_hello_rep {
	__le32 version;		/* the version the daemon speaks: min(requested, its own) */
	__le32 flags;		/* OVNFS_HELLO_* */
	__le32 max_io;		/* largest READ/WRITE payload, at most OVNFS_MAX_IO */
	__le32 pad;
	struct ovnfs_attr root;
};

/* POLL: no body, no result. Exists for its reply header's epoch. */

/* STATFS: no body. */
struct ovnfs_statfs_rep {
	__le64 blocks;		/* in bsize units */
	__le64 bfree;
	__le64 bavail;
	__le64 files;
	__le64 ffree;
	__le32 bsize;
	__le32 namelen;
};

/* GETATTR: no body. Result: struct ovnfs_attr. */

/*
 * SETATTR: changes the attributes named in valid, in the order size, mode,
 * owner, mtime. Result: struct ovnfs_attr after the change.
 */
#define OVNFS_SET_MODE		(1U << 0)
#define OVNFS_SET_UID		(1U << 1)
#define OVNFS_SET_GID		(1U << 2)
#define OVNFS_SET_SIZE		(1U << 3)
#define OVNFS_SET_MTIME		(1U << 4)	/* to mtime_ns */
#define OVNFS_SET_MTIME_NOW	(1U << 5)	/* to the daemon's clock */

struct ovnfs_setattr_req {
	__le32 valid;
	__le32 mode;		/* permission bits only */
	__le32 uid;
	__le32 gid;
	__le64 size;
	__le64 mtime_ns;
};

/* LOOKUP: body is the name. Result: struct ovnfs_attr of the entry. */

/*
 * READDIR: lists a directory in ascending byte order of names, without "."
 * and "..". The body is followed by after_len bytes naming the last entry of
 * the previous page (none for the first page); the listing resumes behind
 * that name whether or not it still exists. The result holds as many whole
 * entries as fit max_bytes (at least one), each padded to a multiple of 8
 * bytes.
 */
struct ovnfs_readdir_req {
	__le32 max_bytes;	/* budget for the result, at most OVNFS_MAX_IO */
	__le16 after_len;
	__le16 pad;
};

struct ovnfs_readdir_rep {
	__le32 count;		/* entries that follow */
	__le32 eof;		/* nonzero: the listing ends with these entries */
};

struct ovnfs_dirent {
	struct ovnfs_attr attr;
	__le16 name_len;
	__u8 name[];		/* name_len bytes, then zero padding to 8 */
};

/* READLINK: no body. Result: the link's target bytes. */

/*
 * READ: result is the data, shorter than len only at end of file. id must be
 * a regular file.
 */
struct ovnfs_read_req {
	__le64 off;
	__le32 len;		/* at most max_io */
	__le32 pad;
};

/*
 * WRITE: the body is followed by len bytes of data. All of it is written or
 * the request fails. Result: struct ovnfs_attr after the write. A write is
 * not durable until FSYNC; the daemon must not sync it on its own.
 */
struct ovnfs_write_req {
	__le64 off;
	__le32 len;		/* at most max_io */
	__le32 pad;
};

/* FSYNC: makes the file's data and its name durable. No body, no result. */

/*
 * CREATE: creates the regular file name in directory id, or opens an existing
 * one unless OVNFS_CREATE_EXCL (then EEXIST). Body is followed by name_len
 * bytes of name and data_len bytes of content. Result: struct ovnfs_attr of
 * the file.
 *
 * A nonzero new_id is the id the guest chose for the file, from ALLOC_IDS.
 * It requires OVNFS_CREATE_EXCL (EINVAL without), and the file is created
 * with that id and with mtime_ns as its mtime, so the result equals what the
 * guest already shows for a file it created ahead of the daemon. An id that
 * was not handed out, or that names a file, is refused with EINVAL; the id is
 * spent either way and the guest never offers it again. With new_id 0 the
 * daemon chooses the id and stamps the file with its own clock.
 *
 * data_len bytes after the name become the new file's content, saving the
 * WRITE and SETATTR a small file would otherwise need; the file keeps
 * mtime_ns. Content needs new_id (EINVAL without) and is at most the max_io
 * of HELLO.
 */
#define OVNFS_CREATE_EXCL	(1U << 0)
#define OVNFS_CREATE_TRUNC	(1U << 1)	/* empty an existing file */

struct ovnfs_create_req {
	__le32 mode;		/* permission bits */
	__le32 flags;		/* OVNFS_CREATE_* */
	__le64 new_id;
	__le64 mtime_ns;	/* with new_id */
	__le32 data_len;	/* with new_id */
	__le16 name_len;
	__le16 pad;
};

/*
 * MKDIR: body is followed by the name. Result: struct ovnfs_attr. new_id and
 * mtime_ns are as for CREATE.
 */
struct ovnfs_mkdir_req {
	__le32 mode;		/* permission bits */
	__le32 pad;
	__le64 new_id;
	__le64 mtime_ns;	/* with new_id */
};

/*
 * MKNOD: creates a FIFO or socket (device nodes are refused with EPERM).
 * Body is followed by the name. Result: struct ovnfs_attr.
 */
struct ovnfs_mknod_req {
	__le32 mode;		/* S_IFIFO or S_IFSOCK plus permission bits */
	__le32 rdev;
};

/*
 * SYMLINK: body is followed by name_len bytes of name, then the target, which
 * is stored verbatim. Result: struct ovnfs_attr of the link.
 */
struct ovnfs_symlink_req {
	__le16 name_len;
	__le16 pad;
	__le32 pad2;
};

/*
 * LINK: gives file id another name in directory dir. Body is followed by the
 * name. Result: struct ovnfs_attr of the file.
 */
struct ovnfs_link_req {
	__le64 dir;
};

/* UNLINK, RMDIR: body is the name in directory id. No result. */

/*
 * RENAME: moves old_len bytes of name in directory id to new_len bytes of
 * name in directory new_dir, replacing a destination unless
 * OVNFS_RENAME_NOREPLACE. Body is followed by the old name, then the new one.
 * No result.
 */
#define OVNFS_RENAME_NOREPLACE	(1U << 0)

struct ovnfs_rename_req {
	__le64 new_dir;
	__le32 flags;		/* OVNFS_RENAME_* */
	__le16 old_len;
	__le16 new_len;
};

/*
 * COMPOUND: several requests in one call, so a batch pays for one round trip.
 * The outer request's id, uid and gid are ignored. Its body is a sequence of
 * sub-requests, each a __le32 byte count followed by that many bytes holding
 * one complete request (struct ovnfs_req and its body; never COMPOUND). The
 * result is the sequence of their replies in the same order, each a __le32
 * byte count followed by a complete reply (struct ovnfs_rep and its result).
 * Sub-requests and replies are packed without padding, so their fields are
 * not aligned.
 *
 * The daemon executes the sub-requests in order, as if they had arrived one
 * by one on this connection, and continues past one that fails: each reply
 * carries its own error. The outer reply's error is nonzero only when the
 * compound itself is malformed (a truncated sub-request, a nested COMPOUND),
 * and then nothing was executed. The whole compound must fit OVNFS_MAX_FRAME.
 * So must its replies: before executing a sub-request the daemon bounds the
 * size of its reply, and one whose reply could not fit what is left of the
 * frame is answered with EMSGSIZE without being executed, while the
 * sub-requests after it still run.
 */

/*
 * ALLOC_IDS: hands the guest ids for files and directories it will create
 * (new_id of CREATE and MKDIR), so that it can create them locally and send
 * the requests later. The request's id is OVNFS_ROOT_ID.
 *
 * The result is a run of count consecutive ids starting at first; count is
 * at least 1 and at most the count asked for. The daemon never gives these
 * ids to anything else, and they stay usable after the daemon restarts and
 * across epoch changes, until the VM is attached to another tree (HELLO's
 * root id changes). Ids the guest never uses are simply lost.
 */
struct ovnfs_alloc_ids_req {
	__le32 count;
	__le32 pad;
};

struct ovnfs_alloc_ids_rep {
	__le64 first;
	__le32 count;
	__le32 pad;
};

#endif /* OVNFS_WIRE_H */
