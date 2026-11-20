//go:build !darwin && !linux && !windows

package base

import "os"

// locator is empty on hosts with no State identity resolver.
type locator struct{}

// seen remembers nothing: see [locator].
func (*locator) seen(os.FileInfo, string) {}

// listed remembers nothing: see [locator].
func (*locator) listed(os.FileInfo, string) {}

// locate reports false: see [locator].
func (c *Caches) locate(*Base, uint64, func(string) bool) (string, bool) { return "", false }
