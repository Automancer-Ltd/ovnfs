package base

import (
	"os"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// locator remembers the paths of State identities handed to clients. WinFsp
// resolves open handles by ID, including when enumerating a directory.
type locator struct{ memo memo }

func (l *locator) lookupInfo(info os.FileInfo, host, p string) os.FileInfo {
	return snapshotInfo{FileInfo: info, host: host, view: p, paths: &l.memo}
}

func (l *locator) listedInfo(info os.FileInfo, host, dir string) os.FileInfo {
	return snapshotInfo{FileInfo: info, host: host, view: dir, listed: true, paths: &l.memo}
}

// locate checks the remembered name against the current fenced or snapshot view.
func (c *Caches) locate(_ *Base, ino uint64, shown func(string) bool) (string, bool) {
	p, ok := c.locator.memo.get(ino)
	return p, ok && shown(p)
}

// snapshotInfo is the metadata of an entry below a root with its identity read on demand.
type snapshotInfo struct {
	os.FileInfo
	host   string
	view   string // the view path, or its parent for a listed entry
	paths  *memo
	listed bool
}

// Identity looks up the NTFS record only when an ID is requested. A caller
// listing just names or attributes needs no per-entry open or path record.
func (i snapshotInfo) Identity() ident.ID {
	_, ino, err := fileIdentity(i.FileInfo, i.host, "")
	if err != nil {
		return 0
	}
	record := ident.NTFSRecord(ino)
	id, ok := ident.New(ident.State, record)
	if ok {
		p := i.view
		if i.listed {
			p += "/" + i.Name() // only directories below a root use listedInfo
		}
		i.paths.put(record, p)
	}
	return id
}
