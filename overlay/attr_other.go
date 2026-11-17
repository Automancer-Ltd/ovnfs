//go:build !darwin && !linux && !windows

package overlay

import "os"

// reusesInodes reports true: nothing promises otherwise on this platform.
func reusesInodes(string) (bool, error) { return true, nil }

// holdsAttr reports false: identities go to the [idTable].
func holdsAttr(os.FileMode) bool { return false }

// readAttr reports no attribute: this platform's files hold none the overlay
// knows how to write.
func readAttr(string, string) (uint64, error) { return 0, nil }

// writeAttr fails with [errNoAttr]: files keep their own inode's identity.
func writeAttr(string, string, uint64) error { return errNoAttr }

// removeAttr has nothing to remove.
func removeAttr(string, string) error { return nil }
