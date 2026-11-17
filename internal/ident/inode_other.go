//go:build !unix

package ident

import "os"

// inode returns the inode number info's native stat carries, if any.
func inode(info os.FileInfo) (uint64, bool) {
	if st, ok := info.Sys().(Inode); ok {
		return st.Inode(), true
	}
	return 0, false
}
