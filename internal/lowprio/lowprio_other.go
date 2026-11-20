//go:build !darwin && !linux && !windows

package lowprio

import "errors"

// Thread reports errors.ErrUnsupported: this platform has no thread priority ovnfs sets.
func Thread() error { return errors.ErrUnsupported }
