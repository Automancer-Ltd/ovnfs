//go:build !darwin && !linux

package overlay

import (
	"os"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// retimed cannot give metadata other times where the native record differs
// from a Unix stat; the host keeps directory times there.
func retimed(os.FileInfo, time.Time, time.Time) (os.FileInfo, bool) { return nil, false }

// queueHost: here the host makes every change at once (see [FS.queueable]).
const queueHost = false

// relinked cannot change a link count where the native record has none.
func relinked(os.FileInfo, int) (os.FileInfo, bool) { return nil, false }

// newDirInfo cannot describe a directory before the host makes it here.
func newDirInfo(string, os.FileMode, time.Time, uint32, uint32, ident.ID) (os.FileInfo, bool) {
	return nil, false
}
