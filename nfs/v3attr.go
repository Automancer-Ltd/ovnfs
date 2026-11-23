package nfs

import (
	"errors"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/internal/unixstat"
)

// maxReportedSizes keeps a reconnect from forgetting a size another
// connection has already reported. A file's size never decreases until
// retire, after which clients see the file through new handles and its
// history starts afresh.
type maxReportedSizes struct {
	mu     sync.Mutex
	retire sync.Mutex // serializes generation changes, never held on a normal stat
	clock  atomic.Uint64
	byID   map[uint64]reportedSize
}

type reportedSize struct {
	size, epoch uint64
	changed     uint64 // clock at the last change in the size clients may be told
}

// store records a new size fence; the caller holds mu.
func (m *maxReportedSizes) store(id uint64, r reportedSize) {
	if m.byID == nil {
		m.byID = make(map[uint64]reportedSize)
	}
	r.changed = m.clock.Add(1)
	m.byID[id] = r
}

// clientReportedSizes lets only the connection that requested a truncate
// observe the smaller size on the same handle.
type clientReportedSizes struct {
	mu      sync.Mutex
	changed *sync.Cond
	byID    map[uint64]requestedSize
	pending map[uint64]bool
}

type requestedSize struct{ size, epoch uint64 }

type sizeCheck uint8

const (
	sizeCurrent sizeCheck = iota
	sizeRaced             // a newer observation or a resize overtook the stat
	sizeShrank            // a current sample is below the permitted size
)

// check both validates and records one sample. A stat collected before a
// completed truncate must not restore the old high-water mark; equally, an
// older GETATTR overtaken by a growing WRITE is not evidence of shrinkage.
func (s *clientReportedSizes) check(m *maxReportedSizes, id, size, sampled uint64) sizeCheck {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wait(id)
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.byID[id]
	floor := r.size
	if req, ok := s.byID[id]; ok {
		if req.epoch == r.epoch {
			floor = req.size
		} else {
			delete(s.byID, id)
		}
	}
	switch {
	case r.changed > sampled:
		return sizeRaced
	case size == floor:
		return sizeCurrent
	case size < floor:
		return sizeShrank
	case size < r.size:
		s.byID[id] = requestedSize{size, r.epoch}
	default:
		r.size = size
		delete(s.byID, id)
	}
	m.store(id, r)
	return sizeCurrent
}

func (s *clientReportedSizes) wait(id uint64) {
	for s.pending[id] {
		if s.changed == nil {
			s.changed = sync.NewCond(&s.mu)
		}
		s.changed.Wait()
	}
}

func (s *clientReportedSizes) begin(id uint64) {
	s.mu.Lock()
	for s.pending[id] {
		if s.changed == nil {
			s.changed = sync.NewCond(&s.mu)
		}
		s.changed.Wait()
	}
	if s.pending == nil {
		s.pending = make(map[uint64]bool)
	}
	s.pending[id] = true
	s.mu.Unlock()
}

func (s *clientReportedSizes) finish(maximum *maxReportedSizes, id, size uint64, accepted bool) {
	s.mu.Lock()
	if accepted {
		maximum.mu.Lock()
		r := maximum.byID[id]
		if size < r.size {
			if s.byID == nil {
				s.byID = make(map[uint64]requestedSize)
			}
			s.byID[id] = requestedSize{size, r.epoch}
		} else {
			r.size = size
			delete(s.byID, id)
		}
		maximum.store(id, r)
		maximum.mu.Unlock()
	}
	delete(s.pending, id)
	if s.changed != nil {
		s.changed.Broadcast()
	}
	s.mu.Unlock()
}

// beginSize holds back concurrent attributes until a client-requested resize
// reply has been sent. That client already updates its VM size safely as part
// of the resize operation.
func (e *encoder) beginSize(id, size uint64) {
	if e.sizes == nil {
		return
	}
	e.sizes.begin(id)
	e.sizeID, e.size, e.sizePending = id, size, true
}

func (e *encoder) acceptSize() { e.sizeAccepted = e.sizePending }

func (e *encoder) finishSize(sent bool) {
	if !e.sizePending {
		return
	}
	accepted := sent && e.sizeAccepted
	e.sizes.finish(e.maxSizes, e.sizeID, e.size, accepted)
	e.sizePending = false
}

// sampleSizes fences attributes collected from here on against later size changes.
func (e *encoder) sampleSizes() {
	if e.maxSizes != nil {
		e.sampled = e.maxSizes.clock.Load()
	}
}

func (e *encoder) checkSize(a *attrs) sizeCheck {
	if a.typ != typeReg || e.sizes == nil || e.sizePending && e.sizeID == a.fileid {
		return sizeCurrent
	}
	return e.sizes.check(e.maxSizes, a.fileid, a.size, e.sampled)
}

// admitSize3 retries a raced sample. Only an unexpected shrink in the same
// live generation retires it; two concurrent page-ins must not retire the
// new handle issued after the first one.
func (s *Server) admitSize3(e *encoder, a *attrs, gen uint64) (retry bool, st status) {
	switch e.checkSize(a) {
	case sizeCurrent:
		return false, stOK
	case sizeRaced:
		return true, stOK
	}
	id, m := a.fileid, e.maxSizes
	m.retire.Lock()
	defer m.retire.Unlock()
	if s.FS.Generation(id) != gen {
		return false, stStale
	}
	// Another observer may have published a change since checkSize.
	m.mu.Lock()
	changed := m.byID[id].changed
	m.mu.Unlock()
	if changed > e.sampled {
		return true, stOK
	}
	s.Recall(id)
	if s.FS.RetireHandle(id) {
		m.mu.Lock()
		m.store(id, reportedSize{epoch: m.byID[id].epoch + 1})
		m.mu.Unlock()
	}
	return false, stStale
}

// fattr3 encodes attributes; callers admit the size before publishing the reply.
func fattr3(e *encoder, a *attrs) {
	e.u32(uint32(a.typ))
	e.u32(a.mode)
	e.u32(a.nlink)
	e.u32(a.uid)
	e.u32(a.gid)
	e.u64(a.size)
	e.u64(a.size) // used
	e.u32(a.major)
	e.u32(a.minor)
	e.u64(0) // fsid: the one export
	e.u64(a.fileid)
	time3(e, a.mtime) // atime
	time3(e, a.mtime)
	time3(e, a.mtime) // ctime
}

// time3 appends t as an nfstime3.
func time3(e *encoder, t time.Time) {
	e.u32(uint32(t.Unix()))
	e.u32(uint32(t.Nanosecond()))
}

// postAttrs3 omits optional attributes rather than advertising bytes past EOF.
func postAttrs3(e *encoder, a *attrs) {
	ok := e.checkSize(a) == sizeCurrent
	e.bool(ok)
	if ok {
		fattr3(e, a)
	}
}

// postOp3 appends the post_op_attr of file id at p, absent when p cannot
// be stat'ed.
func (s *Server) postOp3(e *encoder, id uint64, p string) {
	a, err := s.stat(id, p)
	if err != nil {
		e.bool(false)
		return
	}
	postAttrs3(e, &a)
}

// wcc3 appends wcc_data: the wcc_attr of pre (nil: absent), then the
// current attributes of file id at p.
func (s *Server) wcc3(e *encoder, pre *attrs, id uint64, p string) {
	if pre == nil {
		e.bool(false)
	} else {
		e.bool(true)
		e.u64(pre.size)
		time3(e, pre.mtime)
		time3(e, pre.mtime) // ctime
	}
	s.postOp3(e, id, p)
}

// sattr3 is a decoded sattr3: the attributes a client sets.
type sattr3 struct {
	mode, uid, gid                                       uint32
	size                                                 uint64
	atime, mtime                                         time.Time
	setMode, setUID, setGID, setSize, setAtime, setMtime bool
}

// decodeSattr3 reads an sattr3.
func decodeSattr3(d *decoder) (a sattr3) {
	if a.setMode = d.bool(); a.setMode {
		a.mode = d.u32()
	}
	if a.setUID = d.bool(); a.setUID {
		a.uid = d.u32()
	}
	if a.setGID = d.bool(); a.setGID {
		a.gid = d.u32()
	}
	if a.setSize = d.bool(); a.setSize {
		a.size = d.u64()
	}
	a.atime, a.setAtime = decodeSetTime3(d)
	a.mtime, a.setMtime = decodeSetTime3(d)
	return a
}

// decodeSetTime3 reads a set_atime or set_mtime: false for DONT_CHANGE.
func decodeSetTime3(d *decoder) (time.Time, bool) {
	switch d.u32() {
	case 0: // DONT_CHANGE
		return time.Time{}, false
	case 1: // SET_TO_SERVER_TIME
		return time.Now(), true
	case 2: // SET_TO_CLIENT_TIME
		sec := d.u32()
		nsec := d.u32()
		return time.Unix(int64(sec), int64(nsec)), true
	}
	d.fail()
	return time.Time{}, false
}

// apply3 sets a on the entry at p, which a creating procedure just made.
func (s *Server) apply3(p string, a *sattr3) status {
	info, err := s.FS.Lstat(p)
	if err != nil {
		return statusOf(err)
	}
	return s.applyTo3(s.FS.IDOf(p, info), p, info, a)
}

// applyTo3 sets a on file id, at p, which info describes, as SETATTR and the
// creating procedures do. The mode of a symlink is left alone: its permission bits
// mean nothing, and billy's Chmod follows the link, changing the target or
// failing ENOENT for a dangling one. Ownership changes the link itself
// (Lchown); a size is refused on a symlink.
//
// The size goes first, so a truncate still works when the same call removes
// write permission, and the mode after the owner, which a chown may strip
// of setuid and setgid.
func (s *Server) applyTo3(id uint64, p string, info os.FileInfo, a *sattr3) status {
	link := info.Mode()&os.ModeSymlink != 0
	if a.setSize {
		if link {
			return stNotSupp
		}
		if st := s.truncate3(id, p, a.size); st != stOK {
			return st
		}
	}
	if a.setUID || a.setGID {
		cur, _ := unixstat.Of(info)
		uid, gid := cur.UID, cur.GID
		if a.setUID {
			uid = a.uid
		}
		if a.setGID {
			gid = a.gid
		}
		if uid != cur.UID || gid != cur.GID {
			if err := s.FS.Lchown(p, int(uid), int(gid)); err != nil {
				return statusOf(err)
			}
		}
	}
	if a.setMode && !link && a.mode&0o7777 != posixMode(info.Mode()) {
		if info.Mode().IsRegular() && info.Mode()&0o200 != 0 && a.mode&0o200 == 0 {
			s.keepWritable3(id, p)
		}
		if err := s.FS.Chmod(p, goMode(a.mode)); err != nil {
			return statusOf(err)
		}
	}
	if a.setAtime || a.setMtime {
		atime, mtime := info.ModTime(), info.ModTime()
		if a.setAtime {
			atime = a.atime
		}
		if a.setMtime {
			mtime = a.mtime
		}
		if err := s.FS.Chtimes(p, atime, mtime); err != nil {
			return statusOf(err)
		}
	}
	return stOK
}

// keepWritable3 keeps file id, at p, open for writing before it loses its
// owner's write permission, for writes the client still sends (v3State.kept).
func (s *Server) keepWritable3(id uint64, p string) {
	if s.v3.kept.Has(keptKey{id: id}) {
		return
	}
	if f, _, err := s.openID(id, p, os.O_WRONLY); err == nil {
		s.v3.kept.Keep(keptKey{id: id}, f)
	}
}

// truncate3 sets the size of file id, at p, through its kept file when the
// backend refuses to open it for writing (v3State.kept). Size 0 opens with
// O_TRUNC, so a file from a lower layer is not copied up only to be emptied;
// that open truncates before it can tell whether a rename put another file
// at p, so the caller has just checked that p holds id.
func (s *Server) truncate3(id uint64, p string, size uint64) status {
	if size > math.MaxInt64 {
		return stInval
	}
	flag := os.O_WRONLY
	if size == 0 {
		flag |= os.O_TRUNC
	}
	f, _, err := s.openID(id, p, flag)
	if errors.Is(err, os.ErrPermission) {
		if ok, kerr := s.v3.kept.With(keptKey{id: id}, func(f billy.File) error { return f.Truncate(int64(size)) }); ok {
			return statusOf(kerr)
		}
	}
	if err != nil {
		return statusOf(err)
	}
	if size != 0 {
		err = f.Truncate(int64(size))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return statusOf(err)
}
