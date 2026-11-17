//go:build unix

package ident

import (
	"os"
	"syscall"
)

// inode returns the inode number in info's native stat.
func inode(info os.FileInfo) (uint64, bool) {
	switch st := info.Sys().(type) {
	case *syscall.Stat_t:
		return uint64(st.Ino), true
	case Inode:
		return st.Inode(), true
	}
	return 0, false
}
