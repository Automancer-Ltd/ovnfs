package nfs

import (
	"errors"
	"io"
	"math"
	"os"
	"path"

	"github.com/go-git/go-billy/v5"
)

// getattr3 answers GETATTR (RFC 1813 section 3.3.1).
func (s *Server) getattr3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	a, err := s.stat(id, p)
	if err != nil {
		return statusOf(err)
	}
	if res.sizeRegressed(&a) {
		s.Recall(id)
		if s.FS.RetireHandle(id) {
			s.v3.maxSizes.retire(id)
		}
		return stStale
	}
	fattr3(res, &a)
	return stOK
}

// setattr3 answers SETATTR (section 3.3.2). The guard compares the client's
// ctime with the one GETATTR reports (the mtime).
//
// Like every NFSv3 change, it recalls NFSv4 delegations of the file before
// and again after the change, which catches a delegation granted while the
// change was under way.
func (s *Server) setattr3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	sa := decodeSattr3(d)
	guard := d.bool()
	var sec, nsec uint32
	if guard {
		sec, nsec = d.u32(), d.u32()
	}
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	pre, err := s.stat(id, p)
	if err != nil {
		return statusOf(err)
	}
	if guard && (sec != uint32(pre.mtime.Unix()) || nsec != uint32(pre.mtime.Nanosecond())) {
		return stNotSync
	}
	if sa.setSize {
		res.beginSize(id, sa.size)
	}
	s.recallAttrs(id, pre.typ)
	st = s.apply3(p, &sa)
	s.recallAttrs(id, pre.typ)
	if st != stOK {
		return st
	}
	if sa.setSize {
		res.acceptSize()
	}
	s.wcc3(res, &pre, id, p)
	return stOK
}

// lookup3 answers LOOKUP (section 3.3.3), including "." and "..". The root
// has no parent to look up (ACCES).
//
// A name found leaves out the directory's attributes, which are optional:
// stating it as well took a second host lstat per lookup, and the client
// revalidates its directories on its own schedule. Only a failed lookup
// stats the directory, to tell NOTDIR from the name's own error.
func (s *Server) lookup3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	name := d.str(maxPath3)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	var child string
	switch name {
	case ".", "..":
		dir, err := s.FS.Lstat(p)
		if err != nil {
			return statusOf(err)
		}
		if !dir.IsDir() {
			return stNotDir
		}
		child = p
		if name == ".." {
			if p == "" {
				return stAccess
			}
			child = parentOf(p)
		}
	default:
		if st := checkName3(name); st != stOK {
			return st
		}
		child = path.Join(p, name)
	}
	info, err := s.FS.Lstat(child)
	if err != nil {
		if dir, derr := s.FS.Lstat(p); derr != nil {
			return statusOf(derr)
		} else if !dir.IsDir() {
			return stNotDir
		}
		return statusOf(err)
	}
	fid := s.FS.IDOf(child, info)
	if fid == 0 {
		return stIO
	}
	a := attrsOf(fid, info)
	res.fh(fid, s.FS.Generation(fid))
	postAttrs3(res, &a)
	res.bool(false) // dir_attributes
	return stOK
}

// access3 answers ACCESS (section 3.3.4) with the rights the file's mode
// permits (accessOf). The reply still leaves out the optional attributes:
// the client typically already has them from the LOOKUP or READDIRPLUS
// that preceded the call.
func (s *Server) access3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	mask := d.u32()
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	info, err := s.FS.Lstat(p)
	if err != nil {
		return statusOf(err)
	}
	_, granted := accessOf(info, mask)
	res.bool(false) // obj_attributes
	res.u32(granted)
	return stOK
}

// readlink3 answers READLINK (section 3.3.5); INVAL for a file that is not
// a symlink.
func (s *Server) readlink3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	target, st := s.readlink(p)
	if st != stOK {
		return st
	}
	s.postOp3(res, id, p)
	res.str(target)
	return stOK
}

// read3 answers READ (section 3.3.6), reading straight into the reply. eof
// is set when the data ends at or past the end of the file.
func (s *Server) read3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	off := d.u64()
	count := d.u32()
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	f, err := s.FS.Open(p)
	if err != nil {
		return statusOf(err)
	}
	defer f.Close()
	info, err := s.FS.Stat(p)
	if err != nil {
		return statusOf(err)
	}
	if info.IsDir() {
		return stIsDir
	}
	size := uint64(max(info.Size(), 0))
	var n uint64
	if off < size {
		n = min(uint64(count), size-off, maxIO)
	}
	a := attrsOf(id, info)
	postAttrs3(res, &a)
	at := res.len()
	res.u32(0) // count, set below
	res.u32(0) // eof, set below
	var got int
	if n == 0 {
		res.u32(0)
	} else {
		got, err = res.opaqueFrom(int(n), func(b []byte) (int, error) { return f.ReadAt(b, int64(off)) })
		if err != nil && !errors.Is(err, io.EOF) {
			return statusOf(err)
		}
	}
	res.put32(at, uint32(got))
	if errors.Is(err, io.EOF) || off+uint64(got) >= size {
		res.put32(at+4, 1)
	}
	return stOK
}

// stable_how3 values. DATA_SYNC is implemented as FILE_SYNC.
const (
	unstable3 = 0
	fileSync3 = 2
)

// write3 answers WRITE (section 3.3.7). An unstable write may stay buffered;
// a stable write reaches stable storage before the reply.
func (s *Server) write3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	off := d.u64()
	count := d.u32()
	how := d.u32()
	data := d.opaque(maxRecord)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	if how > fileSync3 {
		return stInval
	}
	if count > math.MaxInt32 || off > math.MaxInt64 {
		return stFBig
	}
	// The filehandle names the object to write, not a path to follow.
	info, err := s.FS.Lstat(p)
	if err != nil {
		return statusOf(err)
	}
	typ := typeOf(info.Mode())
	if typ == typeLnk {
		// A write through a symlink changes its target, not the handle's id.
		if info, err = s.FS.Stat(p); err != nil {
			return statusOf(err)
		}
	}
	if !info.Mode().IsRegular() {
		return stInval
	}
	pre := attrsOf(id, info)
	s.Recall(id)
	n, err := s.writeAt3(id, gen, p, data[:min(len(data), int(count))], int64(off), how != unstable3)
	s.Recall(id)
	if err != nil {
		return statusOf(err)
	}
	s.wcc3(res, &pre, id, p)
	res.u32(uint32(n))
	if how != unstable3 {
		how = fileSync3
	}
	res.u32(how)
	res.fixed(s.verf[:])
	return stOK
}

// writeAt3 reuses the writer for id at p, falling back to a descriptor kept
// before write permission was removed when a fresh open is denied.
func (s *Server) writeAt3(id, gen uint64, p string, data []byte, off int64, stable bool) (int, error) {
	key := keptKey{id: id, path: p}
	var n int
	write := func(f billy.File) (err error) {
		n, err = writeAt(f, data, off)
		if err == nil && stable {
			err = s.FS.SyncFile(p, id, gen, f)
		}
		return err
	}
	if ok, err := s.v3.kept.with(key, write); ok {
		return n, err
	}
	f, err := s.FS.OpenFile(p, os.O_WRONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			if ok, kerr := s.v3.kept.with(keptKey{id: id}, write); ok {
				return n, kerr
			}
		}
		return 0, err
	}
	if err := write(f); err != nil {
		f.Close()
		return n, err
	}
	return n, s.v3.kept.keep(key, f)
}

// commit3 answers COMMIT (section 3.3.21), syncing the whole file even when
// only a range was requested. A kept writer still works after a chmod.
func (s *Server) commit3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	d.u64() // offset
	d.u32() // count
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	flush := func(f billy.File) error { return s.FS.SyncFile(p, id, gen, f) }
	found, err := s.v3.kept.with(keptKey{id: id, path: p}, flush)
	if !found {
		found, err = s.v3.kept.with(keptKey{id: id}, flush)
	}
	if !found {
		err = flush(nil)
	}
	if err != nil {
		return statusOf(err)
	}
	s.wcc3(res, nil, id, p)
	res.fixed(s.verf[:])
	return stOK
}

// fsstat3 answers FSSTAT (section 3.3.18) from Backend.Statfs.
func (s *Server) fsstat3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	fs, err := s.FS.Statfs()
	if err != nil {
		return statusOf(err)
	}
	s.postOp3(res, id, p)
	bsize := uint64(fs.BlockSize)
	res.u64(fs.Blocks * bsize)
	res.u64(fs.Free * bsize)
	res.u64(fs.Avail * bsize)
	res.u64(fs.Files)
	res.u64(fs.FilesFree)
	res.u64(fs.FilesFree) // afiles
	res.u32(0)            // invarsec: the volume changes at any time
	return stOK
}

// FSINFO properties (section 3.3.19).
const (
	fsfLink        = 0x01
	fsfSymlink     = 0x02
	fsfHomogeneous = 0x08
	fsfCanSetTime  = 0x10
)

// fsinfo3 answers FSINFO (section 3.3.19).
func (s *Server) fsinfo3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	s.postOp3(res, id, p)
	res.u32(maxIO) // rtmax
	res.u32(maxIO) // rtpref
	res.u32(4096)  // rtmult
	res.u32(maxIO) // wtmax
	res.u32(maxIO) // wtpref
	res.u32(4096)  // wtmult
	res.u32(8192)  // dtpref
	res.u64(1 << 62)
	res.u32(0) // time_delta: nanoseconds
	res.u32(1)
	res.u32(fsfLink | fsfSymlink | fsfHomogeneous | fsfCanSetTime)
	return stOK
}

// pathconf3 answers PATHCONF (section 3.3.20).
func (s *Server) pathconf3(d *decoder, res *encoder) status {
	id, gen, ok := d.fh(maxFH3)
	if d.err != nil {
		return stInval
	}
	p, st := s.path3(id, gen, ok)
	if st != stOK {
		return st
	}
	s.postOp3(res, id, p)
	res.u32(maxLink)
	res.u32(maxName)
	res.bool(true)  // no_trunc
	res.bool(false) // chown_restricted
	res.bool(false) // case_insensitive
	res.bool(true)  // case_preserving
	return stOK
}
