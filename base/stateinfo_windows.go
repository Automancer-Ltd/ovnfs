package base

import (
	"os"

	"github.com/stencil-hq/ovnfs/internal/ident"
	"golang.org/x/sys/windows"
)

// locator remembers the paths of State identities handed to clients. WinFsp
// resolves open handles by ID, including when enumerating a directory.
type locator struct {
	memo memo
	ntfs bool
}

// newLocator identifies the state volume once per snapshot. Local copies live
// on that volume, and the fence admits cache entries only on the same volume.
func newLocator(state string) locator { return locator{ntfs: isNTFS(state)} }

func isNTFS(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer f.Close()
	var name [windows.MAX_PATH + 1]uint16
	return windows.GetVolumeInformationByHandle(windows.Handle(f.Fd()), nil, 0, nil, nil, nil, &name[0], uint32(len(name))) == nil &&
		windows.UTF16ToString(name[:]) == "NTFS"
}

// stateID preserves every file-index bit on other or unrecognized volumes.
// Unlike NTFS record references, their high bits may distinguish live files.
func (l *locator) stateID(index uint64) (ident.ID, bool) {
	if l.ntfs {
		index = ident.NTFSRecord(index)
	}
	return ident.New(ident.State, index)
}

func (l *locator) lookupInfo(info os.FileInfo, host, p string) os.FileInfo {
	return snapshotInfo{FileInfo: info, host: host, view: p, loc: l}
}

func (l *locator) listedInfo(info os.FileInfo, host, dir string) os.FileInfo {
	return snapshotInfo{FileInfo: info, host: host, view: dir, listed: true, loc: l}
}

// locate checks the remembered name against the current fenced or snapshot view.
func (c *Caches) locate(_ *Base, ino uint64, shown func(string) bool) (string, bool) {
	return c.locator.memo.find(ino, shown)
}

// snapshotInfo is the metadata of an entry below a root with its identity read on demand.
type snapshotInfo struct {
	os.FileInfo
	host   string
	view   string // the view path, or its parent for a listed entry
	loc    *locator
	listed bool
}

// Identity looks up the NTFS record only when an ID is requested. A caller
// listing just names or attributes needs no per-entry open or path record.
func (i snapshotInfo) Identity() ident.ID {
	_, ino, err := fileIdentity(i.FileInfo, i.host, "")
	if err != nil {
		return 0
	}
	id, ok := i.loc.stateID(ino)
	if ok {
		p := i.view
		if i.listed {
			p += "/" + i.Name() // only directories below a root use listedInfo
		}
		i.loc.memo.put(id.Value(), p)
	}
	return id
}
