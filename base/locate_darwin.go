package base

import (
	"bytes"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// locator maps the inode of a cache snapshot file to its path with
// fsgetpath(2), a few microseconds, without walking the snapshot.
type locator struct {
	once sync.Once
	fsid unix.Fsid
	tree string // the snapshot's tree as the kernel names it, symlinks resolved; "" when unknown
}

// locate returns the tree-relative slash path of the snapshot entry with inode ino.
func (c *Caches) locate(ino uint64) (string, bool) {
	l := &c.locator
	l.once.Do(func() {
		var st unix.Statfs_t
		tree, err := filepath.EvalSymlinks(c.tree)
		if err == nil && unix.Statfs(tree, &st) == nil {
			l.fsid, l.tree = st.Fsid, tree
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
	return rel, ok
}
