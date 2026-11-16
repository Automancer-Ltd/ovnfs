//go:build !darwin

package main

// vnodeWarning is always "" off macOS: Linux sizes its inode cache by memory
// pressure and Windows caches files without a fixed count, so neither
// recycles a walk's entries at a cap the way kern.maxvnodes does.
func vnodeWarning(string, int) string { return "" }
