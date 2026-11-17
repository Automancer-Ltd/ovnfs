//go:build !darwin

package base

// locator is empty: only macOS maps an inode to its path (fsgetpath(2)).
// Elsewhere the overlay resolves the State identities of snapshot files from
// the entries it has served.
type locator struct{}

// locate reports false: see [locator].
func (c *Caches) locate(uint64) (string, bool) { return "", false }
