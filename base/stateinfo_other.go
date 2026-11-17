//go:build !windows

package base

import "os"

// stateInfo returns the metadata of cache snapshot file host as served: its
// native stat already carries the inode that is its State identity.
func stateInfo(info os.FileInfo, _ string) os.FileInfo { return info }
