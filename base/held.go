package base

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/stencil-hq/ovnfs/internal/hostfile"
	"github.com/stencil-hq/ovnfs/internal/ident"
	"github.com/stencil-hq/ovnfs/internal/lowprio"
	"github.com/stencil-hq/ovnfs/internal/search/fsys"
	"github.com/stencil-hq/ovnfs/rules"
)

// heldFile records the files below the cache roots as they were at the
// snapshot's fence; see [heldRecord].
const heldFile = "held"

// sumSize is the bytes of a SHA-256 digest a record keeps: enough to tell
// two files apart by their bytes, short enough to keep one per file.
const sumSize = 16

// heldEntry is a file below a cache root as the fence saw it: what identifies
// it in the folder, and the sum of its bytes. Past the fence it also carries
// the change time the record last answered at (0 until it does).
type heldEntry struct {
	ino   uint64
	size  int64
	mtime int64
	typ   fs.FileMode
	sum   [sumSize]byte
	// answered is the change time the record last answered at.
	answered int64
}

// heldEntryOf describes the non-directory entry info, which must carry an
// inode (Unix). Directories are born, not changed: the fence shows them by
// birth time alone.
func heldEntryOf(info os.FileInfo) (heldEntry, bool) {
	id, ok := ident.Of(info)
	if !ok || id.Space() != ident.State || info.IsDir() {
		return heldEntry{}, false
	}
	return heldEntry{ino: id.Value(), size: info.Size(), mtime: info.ModTime().UnixNano(), typ: info.Mode().Type()}, true
}

// heldEntryOfOrZero describes info, or a zero entry that is no file's.
func heldEntryOfOrZero(info os.FileInfo) heldEntry {
	e, _ := heldEntryOf(info)
	return e
}

// same reports whether e and o are the same entry: the folder's same inode,
// type, size and modification time.
func (e heldEntry) same(o heldEntry) bool {
	return e.ino == o.ino && e.typ == o.typ && e.size == o.size && e.mtime == o.mtime
}

// sumOf returns the sum of the bytes of the open file, read from its start.
// It fails when the file is not the entry e when read, or changes while it is
// read: the caller then answers for it as it did before.
func sumOf(file *os.File, e heldEntry) ([sumSize]byte, error) {
	var sum [sumSize]byte
	if e.typ != 0 {
		return sum, nil
	}
	before, err := fsys.FileChangeTime(file)
	if err != nil {
		return sum, err
	}
	info, err := file.Stat()
	now, ok := heldEntryOf(info)
	if err != nil || !ok || !now.same(e) {
		return sum, fs.ErrInvalid
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return sum, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return sum, err
	}
	if after, err := fsys.FileChangeTime(file); err != nil || after != before {
		return sum, fs.ErrInvalid
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// heldReport is one recorded file, as [heldFile] stores it.
type heldReport struct {
	Path     string      `json:"p"`
	Ino      uint64      `json:"i"`
	Size     int64       `json:"s"`
	Mtime    int64       `json:"m"`
	Type     fs.FileMode `json:"t"`
	Sum      string      `json:"u"`
	Answered int64       `json:"a"`
}

// heldRecord holds the files a snapshot's fence showed below the cache roots,
// by view path, with the sum of their bytes. The fence hides a file once its
// change time passes, since that is the only stamp no tool sets back: a file
// made after the fence may carry any mtime, and so may one rewritten in place.
// But a change time also moves when nothing in the file did: another name is
// linked to it or unlinked (a package manager linking a store file into
// another tree, a backup or a cleaned-up worktree), which is the common case
// for the hard-linked files of a dependency tree. So the snapshot sums each
// file it showed, and a file past the fence stays while the record holds its
// path and the file is still the same inode, type, size and mtime with the
// same bytes: the record then answers for it at that change time. Anything
// else past the fence leaves the view as before, and a file whose bytes
// differ is forgotten, so it never comes back.
//
// The record is made by a walk of the cache roots when the snapshot is taken,
// so it holds files the view never looked up. A file changed before the walk
// reaches it is not recorded: the view answers for it as it did before. The
// record is kept in [heldFile] with the snapshot.
type heldRecord struct {
	mu    sync.RWMutex
	files map[string]heldEntry
}

// get returns the record's entry for p.
func (r *heldRecord) get(p string) (heldEntry, bool) {
	r.mu.RLock()
	e, ok := r.files[p]
	r.mu.RUnlock()
	return e, ok
}

// put records p as e.
func (r *heldRecord) put(p string, e heldEntry) {
	r.mu.Lock()
	if r.files == nil {
		r.files = make(map[string]heldEntry)
	}
	r.files[p] = e
	r.mu.Unlock()
}

// forget ends the record of p, if any.
func (r *heldRecord) forget(p string) {
	r.mu.Lock()
	delete(r.files, p)
	r.mu.Unlock()
}

// held reports whether the record answers for the folder's entry that info,
// its lstat data at host, describes at view path p, whose change time is past
// the fence (see [heldRecord]). file is that entry open, when the caller has
// it: its bytes are read through that descriptor, the ones the view serves.
// An answer records the change time it answered at, so a later lookup at the
// same time needs no read. An entry that is no longer the recorded file, or
// whose bytes differ, is forgotten (see [heldRecord]).
func (c *Caches) held(p, host string, info os.FileInfo, file *os.File) bool {
	e, ok := heldEntryOf(info)
	if !ok {
		return false
	}
	old, there := c.heldFiles.get(p)
	if !there {
		return false
	}
	if !old.same(e) {
		c.forget(p)
		return false
	}
	ctime, err := fsys.ChangeTime(host, info)
	if err != nil {
		return false
	}
	if ctime == old.answered && ctime != 0 {
		return true
	}
	if file == nil {
		if file, err = hostfile.Open(host); err != nil {
			return false
		}
		defer file.Close()
	}
	sum, err := sumOf(file, old)
	if err != nil {
		return false
	}
	if sum != old.sum {
		c.forget(p)
		return false
	}
	old.answered = ctime
	c.heldFiles.put(p, old)
	return true
}

// hold records the file at host, view path p, which info describes and the
// fence shows: with the sum of its bytes as they are now, before the fence.
func (c *Caches) hold(p, host string, info os.FileInfo) {
	e, ok := heldEntryOf(info)
	if !ok {
		return
	}
	if old, there := c.heldFiles.get(p); there && old.same(e) && old.sum != [sumSize]byte{} {
		return
	}
	file, err := hostfile.Open(host)
	if err != nil {
		return
	}
	e.sum, err = sumOf(file, e)
	// The bytes are the fence's only once they are read and the file still
	// crosses no fence: the folder may have rewritten it since info was taken.
	var after os.FileInfo
	if err == nil {
		if after, err = file.Stat(); err == nil && !c.fence.shows(host, after) {
			err = fs.ErrInvalid
		}
	}
	file.Close()
	if err != nil {
		return
	}
	c.heldFiles.put(p, e)
}

// walk records the files below the folder's cache roots that the fence shows,
// so that a later link change hides none the view has not looked up yet. It
// runs at the lowest priority the OS offers: it reads every byte below the
// cache roots. The walk ends by keeping its record with the snapshot; it ends
// early, unkept, when the registry stops takes.
func (c *Caches) walk(b *Base) {
	defer close(c.walked)
	runtime.LockOSThread()
	lowprio.Thread()
	prefix := ""
	for _, root := range b.cacheRoots {
		if prefix != "" && strings.HasPrefix(root, prefix+"/") {
			continue
		}
		if class, _ := b.Class(root); class != rules.Cache {
			continue
		}
		if info, ok := b.folderRoot(root); !ok || !info.IsDir() {
			continue
		}
		if c.walkDir(b, root) {
			return
		}
		prefix = root
	}
	c.keep()
}

// walkDir records the files of directory p, at or below a cache root, reporting
// whether the registry stopped takes.
func (c *Caches) walkDir(b *Base, p string) bool {
	if b.stoppingTakes() {
		return true
	}
	entries, err := fsys.ReadDir(b.lower(p))
	if err != nil {
		return false
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.IsDir() {
			if c.walkDir(b, p+"/"+info.Name()) {
				return true
			}
			continue
		}
		if c.fence.shows("", info) {
			c.hold(p+"/"+info.Name(), b.lower(p)+"/"+info.Name(), info)
		}
	}
	return false
}

// keep writes the record below the snapshot's directory, replacing any earlier one.
func (c *Caches) keep() {
	f, err := os.Create(filepath.Join(c.dir, heldFile+".tmp"))
	if err != nil {
		return
	}
	w := bufio.NewWriter(f)
	c.heldFiles.mu.RLock()
	err = nil
	for p, e := range c.heldFiles.files {
		data, _ := json.Marshal(heldReport{Path: p, Ino: e.ino, Size: e.size, Mtime: e.mtime, Type: e.typ, Sum: hex.EncodeToString(e.sum[:]), Answered: e.answered})
		if _, err = w.Write(append(data, '\n')); err != nil {
			break
		}
	}
	c.heldFiles.mu.RUnlock()
	if err == nil {
		err = w.Flush()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(filepath.Join(c.dir, heldFile+".tmp"), filepath.Join(c.dir, heldFile))
	}
	if err != nil {
		os.Remove(filepath.Join(c.dir, heldFile+".tmp"))
	}
}

// heldLoad reads the record the snapshot's walk kept; none is an empty record.
func (c *Caches) heldLoad() {
	f, err := os.Open(filepath.Join(c.dir, heldFile))
	if err != nil {
		return
	}
	defer f.Close()
	files := make(map[string]heldEntry)
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 1<<20)
	for s.Scan() {
		var r heldReport
		if json.Unmarshal(s.Bytes(), &r) != nil {
			continue
		}
		e := heldEntry{ino: r.Ino, size: r.Size, mtime: r.Mtime, typ: r.Type, answered: r.Answered}
		if _, err := hex.Decode(e.sum[:], []byte(r.Sum)); err != nil {
			continue
		}
		files[r.Path] = e
	}
	c.heldFiles.mu.Lock()
	c.heldFiles.files = files
	c.heldFiles.mu.Unlock()
}

// forget ends the record of the file at view path p, which the view answered
// absent, so that no later change brings it back.
func (c *Caches) forget(p string) { c.heldFiles.forget(p) }
