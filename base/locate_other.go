//go:build !darwin && !linux

package base

import "os"

// locator is empty: Windows frontends name files by path, and the file index
// that would identify a snapshot's entry costs an open to read.
type locator struct{}

// seen remembers nothing: see [locator].
func (*locator) seen(os.FileInfo, string) {}

// listed remembers nothing: see [locator].
func (*locator) listed(os.FileInfo, string, string) {}

// locate reports false: see [locator].
func (c *Caches) locate(*Base, uint64, func(string) bool) (string, bool) { return "", false }
