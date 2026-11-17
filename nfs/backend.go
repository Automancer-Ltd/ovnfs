package nfs

import (
	"os"

	"github.com/go-git/go-billy/v5"

	"github.com/stencil-hq/ovnfs/smb"
)

// Backend is the filesystem a Server exports. Paths are overlay-clean and
// relative, "" being the root; every method must be safe for concurrent use.
type Backend interface {
	billy.Filesystem
	billy.Change
	// Link makes newname another name of the file oldname. Errors keep their
	// errno (EXDEV, EMLINK, ENOTSUP, ...), which clients see as statuses.
	Link(oldname, newname string) error
	// FileID returns the id of the entry at p, Lstating it; 0 when p does not
	// exist or has no identity. File handles and fileids carry the id, so it
	// must follow the entry across renames. Callers holding a FileInfo for p
	// use IDOf instead.
	FileID(p string) uint64
	// IDOf returns, without a syscall, the id of the entry at p that info
	// (from this backend's Lstat, Stat or open-file Stat of p) describes; 0
	// when it cannot be determined.
	IDOf(p string, info os.FileInfo) uint64
	// ChildID returns, without a syscall or path join, the id of the entry
	// info, which this backend's ReadDir(dir) returned; READDIRPLUS and
	// NFSv4 READDIR use it for every entry they list.
	ChildID(dir string, info os.FileInfo) uint64
	// Generation returns the tag every handle issued now for id carries.
	// RetireHandle changes it, which makes earlier handles stale.
	Generation(id uint64) uint64
	// Path returns the current path of the file a handle (id, gen) names;
	// false, when the file is gone or gen is not id's current generation,
	// makes the handle stale.
	Path(id, gen uint64) (string, bool)
	// RetireHandle bumps id's generation so its handles go stale while its
	// path and fileid stay; clients then look the name up again. The server
	// calls it when a client caches attributes the file can no longer have.
	// It returns false when id is stale or names the export root.
	RetireHandle(id uint64) bool
	// Cacheable permits NFSv4 read delegations on p. It must return true only
	// when every change to p made outside this server reaches Server.Recall
	// or Server.RecallRange; for a directory, every change to its entries
	// too, so READDIRPLUS can page through a listing without stating each
	// entry again.
	Cacheable(p string) bool
	// Statfs reports the capacity FSSTAT and the NFSv4 space attributes show.
	Statfs() (smb.Statfs, error)
}
