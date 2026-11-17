package base

import (
	"os"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// stateInfo returns the metadata of cache snapshot file host as served. A
// Windows stat carries no file index, so the State identity is read from the
// file when asked for: listings stay one directory query.
func stateInfo(info os.FileInfo, host string) os.FileInfo {
	return snapshotInfo{FileInfo: info, host: host}
}

// snapshotInfo is a cache snapshot entry's metadata with its identity read on demand.
type snapshotInfo struct {
	os.FileInfo
	host string
}

// Identity returns the State identity of the file's volume index; zero when
// the file cannot be read.
func (i snapshotInfo) Identity() ident.ID {
	_, ino, err := fileIdentity(i.FileInfo, i.host, "")
	if err != nil {
		return 0
	}
	id, _ := ident.New(ident.State, ino)
	return id
}
