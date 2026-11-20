package base

import (
	"os"
	"syscall"
)

// locator finds the view path of a State identity, an inode of the state
// volume. Linux maps no inode back to a path without privileges, so it is a
// [memo] of the identities the snapshot handed out. A frontend that names
// files by id (FUSE node ids, NFS handles) resolves through it; an identity
// it no longer holds is stale to that frontend, whose kernel then looks the
// name up again, which remembers it here.
type locator struct {
	memo memo
}

// seen remembers that the entry info describes, with a State identity, was
// looked up at view path p.
func (l *locator) seen(info os.FileInfo, p string) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		l.memo.put(st.Ino, p)
	}
}

// listed remembers the entry named name in a listing of view directory dir:
// a kernel that lists a directory names its entries by id afterwards.
func (l *locator) listed(info os.FileInfo, dir, name string) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		l.memo.put(st.Ino, dir+"/"+name)
	}
}

// locate returns the view path the entry with inode ino was last seen at, if
// the view still shows it there.
func (c *Caches) locate(_ *Base, ino uint64, shown func(p string) bool) (string, bool) {
	p, ok := c.locator.memo.get(ino)
	return p, ok && shown(p)
}
