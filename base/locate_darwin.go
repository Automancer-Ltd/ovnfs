package base

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// locator finds the view path of a State identity, an inode of the state
// volume, with fsgetpath(2): a few microseconds, without walking anything.
// That call names one path of a file with several names, and not always one
// the overlay still shows, so the names of such files that were looked up are
// remembered too (see [memo]) and tried first.
type locator struct {
	once sync.Once
	fsid unix.Fsid
	// The snapshot's tree and the folder as the kernel names them, symlinks
	// resolved; "" when unknown.
	tree, folder string
	memo         memo
}

// seen remembers that the file info describes, if it has several names, was
// looked up at view path p.
func (l *locator) seen(info os.FileInfo, p string) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 && !info.IsDir() {
		l.memo.put(st.Ino, p)
	}
}

// listed remembers nothing: fsgetpath finds a listed entry, and a listed
// name of a file with several may be one the overlay shadows.
func (*locator) listed(os.FileInfo, string) {}

// locate returns a view path of the entry with inode ino that the view
// shows, as shown reports: a file of a local root, in the snapshot's tree, or
// the folder's own file below a cache root.
func (c *Caches) locate(b *Base, ino uint64, shown func(p string) bool) (string, bool) {
	l := &c.locator
	if p, ok := l.memo.get(ino); ok && shown(p) {
		return p, true
	}
	l.once.Do(func() {
		// The base's state exists before any snapshot's tree does.
		var st unix.Statfs_t
		state, err := filepath.EvalSymlinks(b.config.StateDir)
		if err != nil || unix.Statfs(state, &st) != nil {
			return
		}
		l.fsid, l.tree = st.Fsid, filepath.Join(state, cachesDir, c.id, treeDir)
		if c.shared() {
			l.folder, _ = filepath.EvalSymlinks(b.meta.Dir)
		}
	})
	if l.tree == "" {
		return "", false
	}
	var buf [unix.PathMax]byte
	n, _, errno := unix.Syscall6(unix.SYS_FSGETPATH, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&l.fsid)), uintptr(ino), 0, 0)
	if errno != 0 || int(n) > len(buf) {
		return "", false
	}
	found := buf[:n]
	if i := bytes.IndexByte(found, 0); i >= 0 {
		found = found[:i]
	}
	rel, ok := strings.CutPrefix(string(found), l.tree+"/")
	if !ok && l.folder != "" {
		rel, ok = strings.CutPrefix(string(found), l.folder+"/")
	}
	return rel, ok && shown(rel)
}
