//go:build !windows

package base

// Non-Windows builds use the native stat inode without NTFS decoding.
func isNTFS(string) bool { return false }
