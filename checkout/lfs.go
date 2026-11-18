package checkout

import (
	"bytes"
	"io"
	"os"

	"github.com/stencil-hq/ovnfs/base"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// lfsClean returns f, a regular file at p of size bytes that open reads, with
// the base's pointer blob id when it holds exactly the object of the LFS leaf
// the base serves there: content git's clean filter stores as that pointer, so
// the base's own leaf. Anything else, including content it cannot read, keeps
// f, which then merely shows as changed.
func lfsClean(view *base.Base, p string, f store.File, size int64, open func() (io.ReadCloser, error)) store.File {
	if !regular(f) || !view.LFS(p) {
		return f
	}
	if id, ok, err := view.LFSClean(p, size, open); err == nil && ok {
		f.ID = id
	}
	return f
}

// lfsCleanHost is lfsClean for the host file name.
func lfsCleanHost(view *base.Base, p string, f store.File, name string) store.File {
	if !regular(f) || !view.LFS(p) {
		return f
	}
	info, err := os.Lstat(name)
	if err != nil {
		return f
	}
	return lfsClean(view, p, f, info.Size(), func() (io.ReadCloser, error) { return os.Open(name) })
}

// lfsCleanData is lfsClean for content held in memory.
func lfsCleanData(view *base.Base, p string, f store.File, data []byte) store.File {
	return lfsClean(view, p, f, int64(len(data)), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
}

// offeredInfo is the stat data an attach offers for p while the checkout holds
// the base's leaf there: the capture's, except that an LFS leaf goes as the
// pointer blob git stores, not the object the view serves.
func offeredInfo(view *base.Base, p string) (source.Info, bool, error) {
	info, ok := view.Info(p)
	if !ok {
		return info, false, nil
	}
	pointer, lfs, err := view.LFSPointer(p)
	if lfs {
		info.Size = int64(len(pointer))
	}
	return info, true, err
}
