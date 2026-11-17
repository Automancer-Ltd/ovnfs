//go:build !windows

package overlay

import (
	"io/fs"
	"os"
	"syscall"
)

// nativeIdentity: a Unix stat carries the inode [ident.Of] reads, so only a
// persisted identity needs an [idInfo].
const nativeIdentity = true

// nativeKey returns the inode in info's native stat.
func nativeKey(info os.FileInfo) (inodeKey, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return inodeKey{}, false
	}
	return inodeKey{device: uint64(st.Dev), inode: uint64(st.Ino)}, true
}

// fileKey returns the inode of host file name from its Lstat info.
func fileKey(name string, info os.FileInfo) (inodeKey, error) {
	key, ok := nativeKey(info)
	if !ok {
		return inodeKey{}, &fs.PathError{Op: "inode", Path: name, Err: fs.ErrInvalid}
	}
	return key, nil
}

// stateValue is the [ident.State] value of an inode: its number.
func stateValue(key inodeKey) uint64 { return key.inode }

// hardLinks returns the link count in info's native stat; 1 without one.
func hardLinks(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		return uint64(st.Nlink)
	}
	return 1
}
