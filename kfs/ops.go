package kfs

import (
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/go-git/go-billy/v5"

	"github.com/stencil-hq/ovnfs/internal/kept"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// run executes one decoded request; b is its body.
func (c *conn) run(req *Req, b []byte) error {
	d := body{b: b}
	switch req.Op {
	case OpHello:
		version := d.u32()
		d.u32()
		if err := d.end(); err != nil {
			return err
		}
		return c.hello(version)
	case OpPoll:
		return d.end()
	case OpStatfs:
		if err := d.end(); err != nil {
			return err
		}
		return c.statfs()
	case OpGetattr:
		if err := d.end(); err != nil {
			return err
		}
		_, info, err := c.stat(req.ID)
		if err != nil {
			return err
		}
		c.putAttr(req.ID, info)
		return nil
	case OpSetattr:
		var sa SetattrReq
		if err := sa.decode(d.take(SetattrReqSize)); err != nil {
			return EINVAL
		}
		if err := d.end(); err != nil {
			return err
		}
		return c.setattr(req.ID, &sa)
	case OpLookup:
		name, err := nameOf(d.b)
		if err != nil {
			return err
		}
		return c.lookup(req.ID, name)
	case OpReaddir:
		maxBytes := d.u32()
		afterLen := d.u16()
		d.u16()
		after := d.take(int(afterLen))
		if err := d.end(); err != nil {
			return err
		}
		return c.readdir(req.ID, string(after), maxBytes)
	case OpReadlink:
		if err := d.end(); err != nil {
			return err
		}
		return c.readlink(req.ID)
	case OpRead:
		off, n := d.u64(), d.u32()
		d.u32()
		if err := d.end(); err != nil {
			return err
		}
		return c.read(req.ID, off, n)
	case OpWrite:
		off, n := d.u64(), d.u32()
		d.u32()
		data := d.take(int(n))
		if err := d.end(); err != nil {
			return err
		}
		return c.write(req.ID, off, data)
	case OpFsync:
		if err := d.end(); err != nil {
			return err
		}
		return c.fsync(req.ID)
	case OpCreate:
		mode, flags := d.u32(), d.u32()
		name, err := d.name()
		if err != nil {
			return err
		}
		return c.create(req.ID, name, mode, flags)
	case OpMkdir:
		mode := d.u32()
		d.u32()
		name, err := d.name()
		if err != nil {
			return err
		}
		return c.mkdir(req.ID, name, mode)
	case OpMknod:
		mode := d.u32()
		d.u32() // rdev: only FIFOs and sockets are made
		name, err := d.name()
		if err != nil {
			return err
		}
		return c.mknod(req.ID, name, mode)
	case OpSymlink:
		nameLen := d.u16()
		d.u16()
		d.u32()
		raw := d.take(int(nameLen))
		if d.short {
			return EINVAL
		}
		name, err := nameOf(raw)
		if err != nil {
			return err
		}
		return c.symlink(req.ID, name, d.b)
	case OpLink:
		dir := d.u64()
		name, err := d.name()
		if err != nil {
			return err
		}
		return c.link(req.ID, dir, name)
	case OpUnlink, OpRmdir:
		name, err := nameOf(d.b)
		if err != nil {
			return err
		}
		return c.remove(req.ID, name, req.Op == OpRmdir)
	case OpRename:
		var r RenameReq
		if err := r.decode(d.take(RenameReqSize)); err != nil {
			return EINVAL
		}
		oldName, err := nameOf(d.take(int(r.OldLen)))
		if err != nil {
			return err
		}
		newName, err := nameOf(d.take(int(r.NewLen)))
		if err != nil {
			return err
		}
		if err := d.end(); err != nil {
			return err
		}
		return c.rename(req.ID, oldName, r.NewDir, newName, r.Flags)
	}
	return EINVAL
}

// end checks that the body held exactly what its op takes.
func (d *body) end() error {
	if d.short || len(d.b) != 0 {
		return EINVAL
	}
	return nil
}

// name takes the rest of the body as a name.
func (d *body) name() (string, error) {
	if d.short {
		return "", EINVAL
	}
	return nameOf(d.b)
}

// nameOf validates a name: 1 to NameMax bytes, no NUL or '/', not "." or "..".
func nameOf(b []byte) (string, error) {
	switch {
	case len(b) == 0:
		return "", EINVAL
	case len(b) > NameMax:
		return "", ENAMETOOLONG
	case slices.Contains(b, 0), slices.Contains(b, '/'):
		return "", EINVAL
	}
	name := string(b)
	if name == "." || name == ".." {
		return "", EINVAL
	}
	return name, nil
}

// path resolves file id to its export path, which must lie in the checkout.
// An id that names nothing, or something outside, is stale; its kept files
// are dropped.
func (c *conn) path(id uint64) (string, error) {
	if id == RootID {
		return c.share.Checkout, nil
	}
	p, ok := c.nodes.Path(id)
	if ok && c.inside(p) {
		return p, nil
	}
	c.forget(id)
	return "", ESTALE
}

// inside reports whether export path p is the checkout's root or below it.
func (c *conn) inside(p string) bool {
	root := c.share.Checkout
	return p == root || len(p) > len(root) && p[len(root)] == '/' && strings.HasPrefix(p, root)
}

// stat resolves id and describes its file without following a symlink. The
// entry found must still carry id: a rename between resolving and stating
// would otherwise describe whatever took the old name.
func (c *conn) stat(id uint64) (string, os.FileInfo, error) {
	for range 2 {
		p, err := c.path(id)
		if err != nil {
			return "", nil, err
		}
		info, err := c.nodes.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return "", nil, err
		case id == RootID || c.nodes.IDOf(p, info) == id:
			return p, info, nil
		}
	}
	return "", nil, ESTALE
}

// dir resolves directory id for an op that creates in it.
func (c *conn) dir(id uint64) (string, error) {
	p, info, err := c.stat(id)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", ENOTDIR
	}
	return p, nil
}

// missing explains a name that is not in directory id: ENOENT, or ENOTDIR
// when id is no directory.
func (c *conn) missing(id uint64, err error) error {
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, derr := c.dir(id); derr != nil {
		return derr
	}
	return ENOENT
}

// wireID returns the id the guest knows the entry at p, described by info, by.
func (c *conn) wireID(p string, info os.FileInfo) uint64 {
	if p == c.share.Checkout {
		return RootID
	}
	return c.nodes.IDOf(p, info)
}

// putAttr appends the attributes of file id to the reply.
func (c *conn) putAttr(id uint64, info os.FileInfo) {
	a := attrOf(id, info)
	c.out = a.append(c.out)
}

// putAttrAt appends the attributes of the entry at p after a change.
func (c *conn) putAttrAt(p string) error {
	info, err := c.nodes.Lstat(p)
	if err != nil {
		return err
	}
	return c.putAttrFor(p, info)
}

// putAttrFor appends the attributes of the entry at p that info describes.
func (c *conn) putAttrFor(p string, info os.FileInfo) error {
	id := c.wireID(p, info)
	if id == 0 {
		return EIO
	}
	c.putAttr(id, info)
	return nil
}

// attrOf converts host attributes to the wire's, in Linux encoding. ctime
// is reported as mtime, as by every ovnfs frontend.
func attrOf(id uint64, info os.FileInfo) Attr {
	size := uint64(max(info.Size(), 0))
	mtime := uint64(max(info.ModTime().UnixNano(), 0))
	a := Attr{ID: id, Size: size, Blocks: (size + 511) / 512, MtimeNs: mtime, CtimeNs: mtime, Mode: linuxMode(info.Mode()), Nlink: 1}
	if st, ok := unixstat.Of(info); ok {
		if st.Nlink > 0 {
			a.Nlink = st.Nlink
		}
		a.UID, a.GID = st.UID, st.GID
		if info.Mode()&os.ModeDevice != 0 {
			a.Rdev = st.Minor&0xff | st.Major<<8 | (st.Minor&^0xff)<<12 // new_encode_dev
		}
	}
	return a
}

// linuxMode encodes a Go file mode as Linux st_mode.
func linuxMode(m os.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	switch {
	case m.IsDir():
		return bits | sIFDIR
	case m&os.ModeSymlink != 0:
		return bits | sIFLNK
	case m&os.ModeNamedPipe != 0:
		return bits | sIFIFO
	case m&os.ModeSocket != 0:
		return bits | sIFSOCK
	case m&os.ModeCharDevice != 0:
		return bits | sIFCHR
	case m&os.ModeDevice != 0:
		return bits | sIFBLK
	}
	return bits | sIFREG
}

// fileMode converts Linux permission, setuid, setgid and sticky bits to a Go file mode.
func fileMode(bits uint32) os.FileMode {
	m := os.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if bits&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if bits&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// modeBits selects what fileMode can express of a Go file mode.
const modeBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// hello answers HELLO with the version spoken, the share's flags and the root.
func (c *conn) hello(version uint32) error {
	if version == 0 {
		return EINVAL
	}
	_, info, err := c.stat(RootID)
	if err != nil {
		return err
	}
	h := HelloRep{Version: min(version, ProtoVersion), MaxIO: MaxIO, Root: attrOf(RootID, info)}
	if !c.share.Writable {
		h.Flags |= HelloRdonly
	}
	c.out = h.append(c.out)
	return nil
}

// statfs answers STATFS with the capacity of the volume overlay writes land on.
func (c *conn) statfs() error {
	st, err := c.nodes.Statfs()
	if err != nil {
		return err
	}
	r := StatfsRep{Blocks: st.Blocks, Bfree: st.Free, Bavail: st.Avail, Files: st.Files, Ffree: st.FilesFree, Bsize: st.BlockSize, Namelen: NameMax}
	c.out = r.append(c.out)
	return nil
}

// lookup answers LOOKUP: the attributes of name in directory id.
func (c *conn) lookup(id uint64, name string) error {
	p, err := c.path(id)
	if err != nil {
		return err
	}
	child := p + "/" + name
	info, err := c.nodes.Lstat(child)
	if err != nil {
		return c.missing(id, err)
	}
	fid := c.nodes.IDOf(child, info)
	if fid == 0 {
		return EIO
	}
	c.putAttr(fid, info)
	return nil
}

// readlink answers READLINK with the stored target; EINVAL for anything but a symlink.
func (c *conn) readlink(id uint64) error {
	p, info, err := c.stat(id)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return EINVAL
	}
	target, err := c.nodes.Readlink(p)
	if err != nil {
		return err
	}
	c.out = append(c.out, target...)
	return nil
}

// key returns the key of id's kept reader or writer under the request's epoch.
func (c *conn) key(id uint64, write bool) fileKey {
	return fileKey{id: id, write: write, gen: c.gen}
}

// recall tells other protocols that file id, at p, is about to change, as
// the export's Nodes view does for its own changes.
func (c *conn) recall(id uint64, p string) { c.share.Export.Recall(id, p) }

// forget drops id's kept files: it was removed, went stale or was replaced.
func (c *conn) forget(id uint64) {
	c.files.Drop(c.key(id, false))
	c.files.Drop(c.key(id, true))
}

// open opens regular file id, which c.path resolved to p, with flag,
// checking through the open file that it is still id: the name may have
// moved meanwhile, and the overlay would follow a symlink that took it. It
// stats p first, since opening a FIFO would block. A share that may change
// the tree opens past missing owner permission bits: the guest kernel
// checked permissions when its process opened the file.
func (c *conn) open(id uint64, p string, flag int) (billy.File, error) {
	info, err := c.nodes.Lstat(p)
	if err != nil || c.nodes.IDOf(p, info) != id {
		// Moved or removed since it was resolved.
		if p, info, err = c.stat(id); err != nil {
			return nil, err
		}
	}
	switch {
	case info.IsDir():
		return nil, EISDIR
	case !info.Mode().IsRegular():
		return nil, EINVAL
	}
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_TRUNC) != 0 {
		c.recall(id, p)
	}
	f, err := c.openForced(id, p, flag, info.Mode()&modeBits)
	if err != nil {
		return nil, err
	}
	opened, err := statOf(f)
	if err != nil || !opened.Mode().IsRegular() || c.nodes.IDOf(p, opened) != id {
		f.Close()
		return nil, ESTALE
	}
	return f, nil
}

// statOf describes an open file of the export, which every export file can.
func statOf(f billy.File) (os.FileInfo, error) {
	s, ok := f.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return nil, errors.ErrUnsupported
	}
	return s.Stat()
}

// openForced opens file id at p with flag; when that is refused for want of
// the owner's read or write bit on a writable share, it adds the bit for the
// open and restores mode after.
func (c *conn) openForced(id uint64, p string, flag int, mode os.FileMode) (billy.File, error) {
	f, err := c.nodes.OpenFile(p, flag, 0)
	if err == nil || !errors.Is(err, os.ErrPermission) || !c.share.Writable {
		return f, err
	}
	need := os.FileMode(0o400)
	if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
		need |= 0o200
	}
	if mode&need == need {
		return nil, err
	}
	c.recall(id, p)
	if cerr := c.nodes.Chmod(p, mode|need); cerr != nil {
		return nil, err
	}
	f, err = c.nodes.OpenFile(p, flag, 0)
	if rerr := c.nodes.Chmod(p, mode); rerr != nil && err == nil {
		f.Close()
		return nil, rerr
	}
	return f, err
}

// withFile runs fn on id's kept writer, else its kept reader unless write is
// set, else on a file it opens and keeps. Kept files skip the open; they
// follow renames and freezes (export routerFile). The id is resolved first
// either way, so one that went stale is answered as such.
func (c *conn) withFile(id uint64, write bool, flag int, fn func(billy.File) error) error {
	p, err := c.path(id)
	if err != nil {
		return err
	}
	if ok, err := c.files.With(c.key(id, true), fn); ok {
		return err
	}
	if !write {
		if ok, err := c.files.With(c.key(id, false), fn); ok {
			return err
		}
	}
	f, err := c.open(id, p, flag)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		return err
	}
	return c.files.Keep(c.key(id, write), f)
}

// read answers READ, reading straight into the reply.
func (c *conn) read(id, off uint64, n uint32) error {
	if n > MaxIO || off > math.MaxInt64 {
		return EINVAL
	}
	at := len(c.out)
	c.out = slices.Grow(c.out, int(n))
	buf := c.out[at : at+int(n)]
	var got int
	err := c.withFile(id, false, os.O_RDONLY, func(f billy.File) (err error) {
		got, err = f.ReadAt(buf, int64(off))
		if errors.Is(err, io.EOF) {
			err = nil
		}
		return err
	})
	if err != nil {
		return err
	}
	c.out = c.out[:at+got]
	return nil
}

// write answers WRITE without syncing: the data stays wherever the overlay
// puts unsynced writes (its memory tier or the upper) until FSYNC.
func (c *conn) write(id, off uint64, data []byte) error {
	if len(data) > MaxIO {
		return EINVAL
	}
	if off > math.MaxInt64-uint64(len(data)) {
		return EFBIG
	}
	var info os.FileInfo
	err := c.withFile(id, true, os.O_RDWR, func(f billy.File) error {
		n, err := kept.WriteAt(f, data, int64(off))
		if err == nil && n < len(data) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return err
		}
		info, err = statOf(f)
		return err
	})
	// A reader kept on a lower layer would not see the copy-up.
	c.files.Drop(c.key(id, false))
	if err != nil {
		return err
	}
	c.putAttr(id, info)
	return nil
}

// truncate sets the size of regular file id through its kept writer, or one
// it opens and keeps. Size 0 opens with O_TRUNC, so a lower file is not
// copied up only to be emptied.
func (c *conn) truncate(id, size uint64) error {
	if size > math.MaxInt64 {
		return EFBIG
	}
	flag := os.O_RDWR
	if size == 0 {
		flag |= os.O_TRUNC
	}
	err := c.withFile(id, true, flag, func(f billy.File) error { return f.Truncate(int64(size)) })
	c.files.Drop(c.key(id, false))
	return err
}

// fsync answers FSYNC as an NFS COMMIT: the file's data and names reach
// stable storage, through its kept writer when there is one. Directories and
// other files hold no data of their own and need nothing.
func (c *conn) fsync(id uint64) error {
	p, info, err := c.stat(id)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	gen := c.syncer.Generation(id)
	sync := func(f billy.File) error { return c.syncer.SyncFile(p, id, gen, f) }
	if ok, err := c.files.With(c.key(id, true), sync); ok {
		return err
	}
	return sync(nil)
}

// setattr answers SETATTR, applying size, mode, owner and mtime in that order.
func (c *conn) setattr(id uint64, sa *SetattrReq) error {
	const known = SetMode | SetUID | SetGID | SetSize | SetMtime | SetMtimeNow
	if sa.Valid&^known != 0 || sa.Valid&SetMtime != 0 && sa.Valid&SetMtimeNow != 0 || sa.MtimeNs > math.MaxInt64 {
		return EINVAL
	}
	p, info, err := c.stat(id)
	if err != nil {
		return err
	}
	link := info.Mode()&os.ModeSymlink != 0
	if sa.Valid&SetSize != 0 {
		switch {
		case info.IsDir():
			return EISDIR
		case !info.Mode().IsRegular():
			return EINVAL
		}
		c.recall(id, p)
		if err := c.truncate(id, sa.Size); err != nil {
			return err
		}
	}
	if sa.Valid&SetMode != 0 {
		if link {
			return EOPNOTSUPP
		}
		c.recall(id, p)
		if err := c.nodes.Chmod(p, fileMode(sa.Mode)); err != nil {
			return err
		}
	}
	if sa.Valid&(SetUID|SetGID) != 0 {
		uid, gid := -1, -1
		if sa.Valid&SetUID != 0 {
			uid = int(sa.UID)
		}
		if sa.Valid&SetGID != 0 {
			gid = int(sa.GID)
		}
		chown := c.nodes.Chown
		if link {
			chown = c.nodes.Lchown
		}
		c.recall(id, p)
		if err := chown(p, uid, gid); err != nil {
			return err
		}
	}
	if sa.Valid&(SetMtime|SetMtimeNow) != 0 {
		if link {
			return EOPNOTSUPP
		}
		mtime := time.Now()
		if sa.Valid&SetMtime != 0 {
			mtime = time.Unix(0, int64(sa.MtimeNs))
		}
		c.recall(id, p)
		if err := c.nodes.Chtimes(p, time.Time{}, mtime); err != nil {
			return err
		}
	}
	return c.putAttrAt(p)
}

// create answers CREATE: a new regular file, kept open for the writes that
// follow (the guest may have created it without write permission), or an
// existing one unless CreateExcl. It tries the exclusive create first, so a
// new name costs one resolution, one open and one stat of the open file;
// the overlay refuses a directory id that is not one.
func (c *conn) create(dirID uint64, name string, mode, flags uint32) error {
	if flags&^(CreateExcl|CreateTrunc) != 0 {
		return EINVAL
	}
	dir, err := c.path(dirID)
	if err != nil {
		return err
	}
	p := dir + "/" + name
	perm := fileMode(mode)
	for range 2 {
		f, err := c.nodes.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			info, lerr := c.nodes.Lstat(p)
			if errors.Is(lerr, fs.ErrNotExist) {
				continue // removed meanwhile
			}
			if lerr != nil {
				return lerr
			}
			return c.existing(p, info, flags)
		}
		if err != nil {
			return err
		}
		info, err := statOf(f)
		var id uint64
		if err == nil {
			if id = c.nodes.IDOf(p, info); id == 0 {
				err = EIO
			}
		}
		if err == nil && info.Mode()&modeBits != perm {
			// Past the daemon's umask.
			if err = c.nodes.Chmod(p, perm); err == nil {
				info, err = statOf(f)
			}
		}
		if err != nil {
			f.Close()
			return err
		}
		if err := c.files.Keep(c.key(id, true), f); err != nil {
			return err
		}
		c.putAttr(id, info)
		return nil
	}
	return EEXIST
}

// existing answers a CREATE of name p that exists, described by info.
func (c *conn) existing(p string, info os.FileInfo, flags uint32) error {
	switch {
	case flags&CreateExcl != 0:
		return EEXIST
	case info.IsDir():
		return EISDIR
	case !info.Mode().IsRegular():
		return EEXIST
	}
	id := c.nodes.IDOf(p, info)
	if id == 0 {
		return EIO
	}
	if flags&CreateTrunc == 0 || info.Size() == 0 {
		c.putAttr(id, info)
		return nil
	}
	if err := c.truncate(id, 0); err != nil {
		return err
	}
	return c.putAttrAt(p)
}

// newName resolves directory id and refuses name there if it is taken, for
// the creations whose overlay call would not (MkdirAll succeeds on an
// existing directory).
func (c *conn) newName(dirID uint64, name string) (string, error) {
	dir, err := c.path(dirID)
	if err != nil {
		return "", err
	}
	p := dir + "/" + name
	if _, err := c.nodes.Lstat(p); err == nil {
		return "", EEXIST
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	return p, nil
}

// mkdir answers MKDIR.
func (c *conn) mkdir(dirID uint64, name string, mode uint32) error {
	p, err := c.newName(dirID, name)
	if err != nil {
		return err
	}
	perm := fileMode(mode)
	if err := c.nodes.MkdirAll(p, perm); err != nil {
		return err
	}
	info, err := c.nodes.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&modeBits != perm {
		// Past the daemon's umask.
		if err := c.nodes.Chmod(p, perm); err != nil {
			return err
		}
		return c.putAttrAt(p)
	}
	return c.putAttrFor(p, info)
}

// mknod answers MKNOD for FIFOs and sockets; device nodes are EPERM.
func (c *conn) mknod(dirID uint64, name string, mode uint32) error {
	var kind os.FileMode
	switch mode & sIFMT {
	case sIFIFO:
		kind = os.ModeNamedPipe
	case sIFSOCK:
		kind = os.ModeSocket
	case sIFCHR, sIFBLK:
		return EPERM
	default:
		return EINVAL
	}
	dir, err := c.path(dirID)
	if err != nil {
		return err
	}
	p := dir + "/" + name
	perm := fileMode(mode)
	if err := c.nodes.Mknod(p, kind|perm); err != nil {
		return err
	}
	info, err := c.nodes.Lstat(p)
	if err != nil {
		return err
	}
	if info.Mode()&modeBits != perm {
		if err := c.nodes.Chmod(p, perm); err != nil {
			return err
		}
		return c.putAttrAt(p)
	}
	return c.putAttrFor(p, info)
}

// symlinkMax bounds a symlink target, as Linux's PATH_MAX does.
const symlinkMax = 4095

// symlink answers SYMLINK, storing target verbatim.
func (c *conn) symlink(dirID uint64, name string, target []byte) error {
	switch {
	case len(target) == 0:
		return ENOENT
	case len(target) > symlinkMax:
		return ENAMETOOLONG
	case slices.Contains(target, 0):
		return EINVAL
	}
	dir, err := c.path(dirID)
	if err != nil {
		return err
	}
	p := dir + "/" + name
	if err := c.nodes.Symlink(string(target), p); err != nil {
		return err
	}
	return c.putAttrAt(p)
}

// link answers LINK: file id gets name in directory dirID. The overlay
// refuses a name taken.
func (c *conn) link(id, dirID uint64, name string) error {
	from, info, err := c.stat(id)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return EPERM
	}
	dir, err := c.path(dirID)
	if err != nil {
		return err
	}
	p := dir + "/" + name
	c.recall(id, from) // its link count changes
	if err := c.nodes.Link(from, p); err != nil {
		return err
	}
	return c.putAttrAt(p)
}

// remove answers UNLINK, or RMDIR with dir set, dropping the files kept
// for what it removes.
func (c *conn) remove(dirID uint64, name string, dir bool) error {
	parent, err := c.path(dirID)
	if err != nil {
		return err
	}
	p := parent + "/" + name
	info, err := c.nodes.Lstat(p)
	if err != nil {
		return c.missing(dirID, err)
	}
	switch {
	case dir && !info.IsDir():
		return ENOTDIR
	case !dir && info.IsDir():
		return EISDIR
	}
	if id := c.nodes.IDOf(p, info); id != 0 {
		c.forget(id)
		c.recall(id, p)
	}
	return c.nodes.Remove(p)
}

// rename answers RENAME. NoReplace is checked before the move, not atomically
// with it: the overlay has no renameat2. A move within one directory
// resolves it once; the overlay refuses a target directory id that is not one.
func (c *conn) rename(oldDir uint64, oldName string, newDir uint64, newName string, flags uint32) error {
	if flags&^RenameNoreplace != 0 {
		return EINVAL
	}
	parent, err := c.path(oldDir)
	if err != nil {
		return err
	}
	target := parent
	if newDir != oldDir {
		if target, err = c.path(newDir); err != nil {
			return err
		}
	}
	from, to := parent+"/"+oldName, target+"/"+newName
	if from == to {
		return nil
	}
	moved, err := c.nodes.Lstat(from)
	if err != nil {
		return c.missing(oldDir, err)
	}
	replaced, err := c.nodes.Lstat(to)
	switch {
	case err == nil && flags&RenameNoreplace != 0:
		return EEXIST
	case err == nil:
		if id := c.nodes.IDOf(to, replaced); id != 0 {
			c.forget(id)
			c.recall(id, to)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	c.recall(c.nodes.IDOf(from, moved), from)
	return c.nodes.Rename(from, to)
}

// listing is a directory's entries, sorted by name, as one read of it
// returned. A connection keeps its last one, so paging through a large
// directory reads it once.
type listing struct {
	dir     uint64
	gen     uint64 // the epoch generation it was read under
	changes uint64 // server.changes before it was read
	entries []Dirent
}

// readdir answers READDIR: the entries after the name after, as many as
// maxBytes holds and at least one. A first page reads the directory afresh;
// later ones reuse the connection's listing of it while neither this share
// nor the daemon changed the tree since it was read.
func (c *conn) readdir(id uint64, after string, maxBytes uint32) error {
	if maxBytes > MaxIO || maxBytes < ReaddirRepSize {
		return EINVAL
	}
	p, err := c.path(id)
	if err != nil {
		return err
	}
	changes := c.changes.Load()
	l := c.list
	if after == "" || l == nil || l.dir != id || l.gen != c.gen || l.changes != changes {
		c.list = nil
		if l, err = c.readList(id, p, changes); err != nil {
			return err
		}
		c.list = l
	}
	i, _ := slices.BinarySearchFunc(l.entries, after, func(e Dirent, name string) int { return strings.Compare(e.Name, name) })
	if i < len(l.entries) && l.entries[i].Name == after {
		i++
	}
	at := len(c.out)
	c.out = append(c.out, make([]byte, ReaddirRepSize)...)
	budget := int(maxBytes) - ReaddirRepSize
	count := 0
	for ; i < len(l.entries); i++ {
		size := direntSize(len(l.entries[i].Name))
		if count > 0 && size > budget {
			break
		}
		c.out = l.entries[i].append(c.out)
		budget -= size
		count++
	}
	le.PutUint32(c.out[at:], uint32(count))
	if i == len(l.entries) {
		le.PutUint32(c.out[at+4:], 1)
	}
	return nil
}

// readList reads directory id at p. Entries take their ids from the listing
// without a stat each (Nodes.ChildID); names the protocol cannot carry are
// left out.
func (c *conn) readList(id uint64, p string, changes uint64) (*listing, error) {
	infos, err := c.nodes.ReadDir(p)
	if err != nil {
		return nil, err
	}
	l := &listing{dir: id, gen: c.gen, changes: changes, entries: make([]Dirent, 0, len(infos))}
	for _, info := range infos {
		name := info.Name()
		if _, err := nameOf([]byte(name)); err != nil {
			continue
		}
		fid := c.nodes.ChildID(p, info)
		if fid == 0 {
			continue
		}
		l.entries = append(l.entries, Dirent{Attr: attrOf(fid, info), Name: name})
	}
	slices.SortFunc(l.entries, func(a, b Dirent) int { return strings.Compare(a.Name, b.Name) })
	return l, nil
}
