package export

import (
	"os"
	"strings"

	"github.com/stencil-hq/ovnfs/overlay"
	"github.com/stencil-hq/ovnfs/smb"
)

// protocolFS is the export as a network protocol server sees it: the
// routed filesystem without the recall hook (each server recalls its own
// clients' caches), file ids, the caching policy and volume capacity.
type protocolFS struct {
	*nodeFS
	volume string
}

// protocolFS shares e's filesystem, without its recall hook.
func (e *Export) protocolFS() protocolFS {
	return protocolFS{
		nodeFS: &nodeFS{Filesystem: e.fs.Filesystem, Change: e.fs.Change, rt: e.rt},
		volume: e.volume,
	}
}

// FileID returns the file id of the entry at p, which NFS handles and SMB
// file ids carry; 0 when there is none, which must not be published. It
// costs an Lstat: callers with p's FileInfo use IDOf.
func (s *protocolFS) FileID(p string) uint64 { return s.rt.id(p) }

// IDOf returns the file id of the entry at p that info, from this backend's
// Lstat, Stat or open file Stat of p, describes, without a syscall.
func (s *protocolFS) IDOf(p string, info os.FileInfo) uint64 { return s.rt.idOf(p, info) }

// Cacheable permits leases and delegations on any path inside a checkout:
// nothing changes a checkout behind the file protocols except the daemon,
// whose changes reach the protocols as recalls (Export.Invalidate).
func (s *protocolFS) Cacheable(p string) bool {
	_, _, ok := strings.Cut(overlay.Clean(p), "/")
	return ok
}

// Statfs reports space on upper's volume, where all overlay writes land.
func (s *protocolFS) Statfs() (smb.Statfs, error) { return volumeStatfs(s.volume) }

// smbFS is the export as the SMB server sees it.
type smbFS struct {
	protocolFS
	writeCacheable func(checkout, p string) bool
}

// SMB exposes stable file identities; SMB itself recalls conflicting client caches.
func (e *Export) SMB() smb.Backend {
	return &smbFS{protocolFS: e.protocolFS(), writeCacheable: e.rt.reg.WriteCacheable}
}

// WriteCacheable admits isolated upper files whose accesses are covered by
// the daemon's pre-operation barriers, never lower paths or hard-link aliases.
func (s *smbFS) WriteCacheable(p string) bool {
	co, rel, ok := strings.Cut(overlay.Clean(p), "/")
	return ok && s.writeCacheable(co, rel)
}

// The adapter exposes filesystem operations and metadata mutations.
var _ smb.Backend = (*smbFS)(nil)
