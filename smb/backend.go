package smb

import (
	"os"

	"github.com/go-git/go-billy/v5"
)

// Backend serves overlay-clean relative paths and stable file identifiers.
type Backend interface {
	billy.Filesystem
	billy.Change
	// FileID Lstats p and returns its stable file id, 0 when it has none;
	// used where no FileInfo for the entry is at hand.
	FileID(p string) uint64
	// IDOf returns the file id of the entry at p that info (from this
	// backend's Lstat/Stat of p) describes, without a syscall; 0 when
	// undeterminable.
	IDOf(p string, info os.FileInfo) uint64
	// Cacheable permits caching only when backend changes can call Server.Changed.
	Cacheable(p string) bool
	Statfs() (Statfs, error)
}

// Statfs describes filesystem capacity in units of BlockSize bytes.
type Statfs struct {
	BlockSize                             uint32
	Blocks, Free, Avail, Files, FilesFree uint64
}
