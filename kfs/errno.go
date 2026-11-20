package kfs

import (
	"errors"
	"os"
	"strconv"
	"syscall"
)

// Errno is a Linux errno number, as ovnfs_rep.error carries it whatever the
// daemon's host: macOS and Windows number their errors differently.
type Errno uint32

// Linux errno numbers (asm-generic/errno-base.h, asm-generic/errno.h).
const (
	EPERM        Errno = 1
	ENOENT       Errno = 2
	EIO          Errno = 5
	EAGAIN       Errno = 11
	EACCES       Errno = 13
	EBUSY        Errno = 16
	EEXIST       Errno = 17
	EXDEV        Errno = 18
	ENOTDIR      Errno = 20
	EISDIR       Errno = 21
	EINVAL       Errno = 22
	ETXTBSY      Errno = 26
	EFBIG        Errno = 27
	ENOSPC       Errno = 28
	EROFS        Errno = 30
	EMLINK       Errno = 31
	ENAMETOOLONG Errno = 36
	ENOTEMPTY    Errno = 39
	ELOOP        Errno = 40
	EOPNOTSUPP   Errno = 95
	ESTALE       Errno = 116
	EDQUOT       Errno = 122
)

// errnoNames are the symbolic names Error reports.
var errnoNames = map[Errno]string{
	EPERM: "EPERM", ENOENT: "ENOENT", EIO: "EIO", EAGAIN: "EAGAIN", EACCES: "EACCES",
	EBUSY: "EBUSY", EEXIST: "EEXIST", EXDEV: "EXDEV", ENOTDIR: "ENOTDIR", EISDIR: "EISDIR",
	EINVAL: "EINVAL", ETXTBSY: "ETXTBSY", EFBIG: "EFBIG", ENOSPC: "ENOSPC", EROFS: "EROFS",
	EMLINK: "EMLINK", ENAMETOOLONG: "ENAMETOOLONG", ENOTEMPTY: "ENOTEMPTY", ELOOP: "ELOOP",
	EOPNOTSUPP: "EOPNOTSUPP", ESTALE: "ESTALE", EDQUOT: "EDQUOT",
}

// Error names the errno.
func (e Errno) Error() string {
	if name, ok := errnoNames[e]; ok {
		return "kfs: " + name
	}
	return "kfs: errno " + strconv.FormatUint(uint64(e), 10)
}

// errnoOf maps an error of the export, the overlay or the host to the Linux
// errno the guest sees; EIO when nothing closer fits. Order matters:
// os.ErrExist matches ENOTEMPTY on some hosts and os.ErrPermission matches
// EPERM.
func errnoOf(err error) Errno {
	var own Errno
	switch {
	case err == nil:
		return 0
	case errors.As(err, &own):
		return own
	case errors.Is(err, syscall.EXDEV):
		return EXDEV
	case notEmpty(err):
		return ENOTEMPTY
	case errors.Is(err, os.ErrExist):
		return EEXIST
	case errors.Is(err, syscall.EPERM):
		return EPERM
	case errors.Is(err, os.ErrPermission):
		return EACCES
	case errors.Is(err, syscall.EROFS):
		return EROFS
	case errors.Is(err, syscall.ESTALE), errors.Is(err, os.ErrClosed):
		// A kept file whose checkout was retired reports itself closed.
		return ESTALE
	case errors.Is(err, os.ErrNotExist):
		return ENOENT
	case errors.Is(err, syscall.ENOTDIR):
		return ENOTDIR
	case errors.Is(err, syscall.EISDIR):
		return EISDIR
	case errors.Is(err, syscall.ENAMETOOLONG):
		return ENAMETOOLONG
	case errors.Is(err, syscall.ELOOP):
		return ELOOP
	case errors.Is(err, syscall.EMLINK):
		return EMLINK
	case errors.Is(err, syscall.ENOSPC):
		return ENOSPC
	case errors.Is(err, syscall.EDQUOT):
		return EDQUOT
	case errors.Is(err, syscall.EFBIG):
		return EFBIG
	case errors.Is(err, syscall.ETXTBSY):
		return ETXTBSY
	case errors.Is(err, syscall.EBUSY):
		return EBUSY
	case errors.Is(err, syscall.EINVAL):
		return EINVAL
	case errors.Is(err, errors.ErrUnsupported), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP):
		return EOPNOTSUPP
	}
	return EIO
}
