//go:build !windows

package base

import "os"

// newLocator needs no volume-specific setup for native inode numbers.
func newLocator(_ string) locator { return locator{} }

// lookupInfo and listedInfo return the native stat, which carries a State
// entry's inode, remembering its view path where needed for Locate.
func (l *locator) lookupInfo(info os.FileInfo, _, p string) os.FileInfo {
	l.seen(info, p)
	return info
}

func (l *locator) listedInfo(info os.FileInfo, _, dir string) os.FileInfo {
	l.listed(info, dir)
	return info
}
