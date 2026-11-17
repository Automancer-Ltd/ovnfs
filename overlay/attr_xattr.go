//go:build darwin || linux

package overlay

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

// attrName is the extended attribute holding attribute a (see attrPrefix).
func attrName(a string) string { return attrPrefix + a }

// readAttr returns attribute a of host file name, without following a final
// symlink; 0 when the file has none.
func readAttr(name, a string) (uint64, error) {
	var buf [8]byte
	n, err := unix.Lgetxattr(name, attrName(a), buf[:])
	switch {
	case errors.Is(err, errNoXattr), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
		return 0, nil
	case err != nil:
		return 0, &fs.PathError{Op: "getxattr", Path: name, Err: err}
	case n != len(buf):
		return 0, &fs.PathError{Op: "getxattr", Path: name, Err: errors.New("overlay: malformed attribute " + attrName(a))}
	}
	return binary.BigEndian.Uint64(buf[:]), nil
}

// writeAttr sets attribute a of host file name, without following a final
// symlink. Linux checks write permission for user attributes, so a file
// without owner write permission gets it for the write. It fails with
// [errNoAttr] where the file cannot hold the attribute.
func writeAttr(name, a string, v uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	err := unix.Lsetxattr(name, attrName(a), buf[:], 0)
	if errors.Is(err, unix.EACCES) {
		info, statErr := os.Lstat(name)
		if statErr == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o200 == 0 {
			if chmodErr := os.Chmod(name, info.Mode().Perm()|0o200); chmodErr == nil {
				err = unix.Lsetxattr(name, attrName(a), buf[:], 0)
				err = errors.Join(err, restoreMode(name, info.Mode()))
			}
		}
	}
	switch {
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), refusesXattr(err):
		return errNoAttr
	case err != nil:
		return &fs.PathError{Op: "setxattr", Path: name, Err: err}
	}
	return nil
}

// removeAttr removes attribute a of host file name, if it has one, without
// following a final symlink.
func removeAttr(name, a string) error {
	err := unix.Lremovexattr(name, attrName(a))
	if errors.Is(err, unix.EACCES) {
		info, statErr := os.Lstat(name)
		if statErr == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o200 == 0 {
			if chmodErr := os.Chmod(name, info.Mode().Perm()|0o200); chmodErr == nil {
				err = unix.Lremovexattr(name, attrName(a))
				err = errors.Join(err, restoreMode(name, info.Mode()))
			}
		}
	}
	switch {
	case errors.Is(err, errNoXattr), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), refusesXattr(err):
		return nil
	case err != nil:
		return &fs.PathError{Op: "removexattr", Path: name, Err: err}
	}
	return nil
}

// restoreMode puts back the mode writeAttr widened, setuid, setgid and sticky
// bits included.
func restoreMode(name string, mode os.FileMode) error {
	return os.Chmod(name, mode&memModeBits)
}
