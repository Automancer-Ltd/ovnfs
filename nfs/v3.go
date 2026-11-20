package nfs

import (
	"strings"

	"github.com/stencil-hq/ovnfs/internal/kept"
)

// NFSv3 procedures (RFC 1813 section 3).
const (
	v3Null = iota
	v3GetAttr
	v3SetAttr
	v3Lookup
	v3Access
	v3Readlink
	v3Read
	v3Write
	v3Create
	v3Mkdir
	v3Symlink
	v3Mknod
	v3Remove
	v3Rmdir
	v3Rename
	v3Link
	v3ReadDir
	v3ReadDirPlus
	v3FSStat
	v3FSInfo
	v3PathConf
	v3Commit
	// v3Count is the number of procedures.
	v3Count
)

// stNotSync is NFS3ERR_NOT_SYNC: a SETATTR guard did not match.
const stNotSync status = 10002

// NFSv3 limits.
const (
	// maxFH3 is NFS3_FHSIZE, the longest handle a client may send.
	maxFH3 = 64
	// maxPath3 bounds names and symlink targets as decoded; longer names
	// than maxName decode and fail NAMETOOLONG.
	maxPath3 = 4096
)

// v3State is what the server remembers between NFSv3 calls, a stateless
// protocol: cached writers and permission fallbacks (kept), the
// verifiers of EXCLUSIVE creates, the calls under way per file
// (fileOrder) and the directories whose entries change (dirLocks).
type v3State struct {
	// kept retains writers across bursts of WRITE requests instead of
	// opening and closing a host descriptor for each small write. Files
	// opened before CREATE or SETATTR removes write permission also stay
	// here: clients may send their buffered writes afterwards. NFSv4 keeps
	// those permission fallbacks per open instead (keepWritable).
	kept     kept.Cache[keptKey]
	verfs    createVerfs
	maxSizes maxReportedSizes
	order    fileOrder
	dirs     dirLocks
}

// v3proc is an NFSv3 procedure. run appends the result after the NFS3_OK
// status that nfs3 writes; on another status nfs3 replaces it with the
// failure arm: the status and fail bytes of zeros, the discriminants of the
// absent post_op_attr and wcc_data fields the procedure's resfail carries.
// When the arguments do not decode, run returns without acting and its
// status is ignored.
type v3proc struct {
	name string // as RFC 1813 spells it, which names its Server.Stats timer
	run  func(s *Server, d *decoder, res *encoder) status
	fail int
	// namespace marks changes that can affect multiple entries. SETATTR and
	// WRITE record their affected object themselves.
	namespace bool
}

// v3procs are the procedures by number; NULL is answered by nfs3 itself.
var v3procs = [v3Count]v3proc{
	v3GetAttr:     {"GETATTR", (*Server).getattr3, 0, false},
	v3SetAttr:     {"SETATTR", (*Server).setattr3, 8, false},
	v3Lookup:      {"LOOKUP", (*Server).lookup3, 4, false},
	v3Access:      {"ACCESS", (*Server).access3, 4, false},
	v3Readlink:    {"READLINK", (*Server).readlink3, 4, false},
	v3Read:        {"READ", (*Server).read3, 4, false},
	v3Write:       {"WRITE", (*Server).write3, 8, false},
	v3Create:      {"CREATE", (*Server).create3, 8, true},
	v3Mkdir:       {"MKDIR", (*Server).mkdir3, 8, true},
	v3Symlink:     {"SYMLINK", (*Server).symlink3, 8, true},
	v3Mknod:       {"MKNOD", (*Server).mknod3, 8, true},
	v3Remove:      {"REMOVE", (*Server).remove3, 8, true},
	v3Rmdir:       {"RMDIR", (*Server).rmdir3, 8, true},
	v3Rename:      {"RENAME", (*Server).rename3, 16, true},
	v3Link:        {"LINK", (*Server).link3, 12, true},
	v3ReadDir:     {"READDIR", (*Server).readdir3, 4, false},
	v3ReadDirPlus: {"READDIRPLUS", (*Server).readdirplus3, 4, false},
	v3FSStat:      {"FSSTAT", (*Server).fsstat3, 4, false},
	v3FSInfo:      {"FSINFO", (*Server).fsinfo3, 4, false},
	v3PathConf:    {"PATHCONF", (*Server).pathconf3, 4, false},
	v3Commit:      {"COMMIT", (*Server).commit3, 8, false},
}

// nfs3 answers NFSv3 calls.
func (s *Server) nfs3(c *call, res *encoder) acceptStat {
	if c.proc == v3Null {
		return rpcSuccess
	}
	if c.proc >= uint32(len(v3procs)) {
		return rpcProcUnavail
	}
	p := v3procs[c.proc]
	at := res.len()
	res.u32(uint32(stOK))
	timer := s.timers.v3[c.proc]
	began := timer.Start()
	st := p.run(s, &c.args, res)
	timer.Since(began)
	if p.namespace {
		s.changes.invalidate()
	}
	if c.args.err != nil {
		return rpcGarbageArgs
	}
	if st != stOK {
		res.sizeAccepted = false
		res.truncate(at)
		res.u32(uint32(st))
		for range p.fail / 4 {
			res.u32(0)
		}
	}
	return rpcSuccess
}

// path3 resolves a decoded file handle: BADHANDLE for one no server can have
// issued, STALE for one resolve refuses.
func (s *Server) path3(id, gen uint64, ok bool) (string, status) {
	if !ok {
		return "", stBadHandle
	}
	p, _, live := s.resolve(id, gen)
	if !live {
		return "", stStale
	}
	return p, stOK
}

// resolve finds the current path of the handle id with generation gen, and
// the generation its current handles carry: false when its file is gone,
// its generation is no longer current, or it is a legacy handle of anything
// but the export root. The root's legacy handle stays valid so a mount kept
// across the handle-format upgrade keeps working: everything below it is
// looked up again.
func (s *Server) resolve(id, gen uint64) (string, uint64, bool) {
	if id == 0 {
		return "", 0, false
	}
	if gen == legacyGen {
		if id != s.FS.FileID("") {
			return "", 0, false
		}
		gen = s.FS.Generation(id)
	}
	p, live := s.FS.Path(id, gen)
	return p, gen, live
}

// checkName3 vets a file name a client sends. Empty names and names with a
// slash or NUL get ACCES, as Linux's nfsd answers (lookup_one_len refuses
// them with EACCES); "." and ".." are the callers' to treat.
func checkName3(name string) status {
	switch {
	case len(name) > maxName:
		return stNameTooLong
	case name == "", strings.ContainsAny(name, "/\x00"):
		return stAccess
	}
	return stOK
}

// isDot reports the names that refer to a directory itself or its parent,
// which name no entry a procedure could create or remove.
func isDot(name string) bool { return name == "." || name == ".." }

// parentOf is the directory containing p; the root is its own parent.
func parentOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}
