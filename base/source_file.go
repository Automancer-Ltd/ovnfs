package base

import (
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/stencil-hq/ovnfs/internal/clone"
	"github.com/stencil-hq/ovnfs/overlay"
)

// sourceFile reads a captured source inode directly until its size or mtime
// no longer matches. Validate after the read, not just on open: a truncate
// between those calls must not publish an early EOF or newly written bytes.
// The first mismatch switches this handle to the captured content, keeping
// its logical position independent of both backing descriptors.
type sourceFile struct {
	b    *Base
	path string
	node uint32
	info os.FileInfo // open-time stat, matching the capture

	mu       sync.Mutex
	host     *os.File
	captured overlay.Content
	off      int64
}

// intact checks the open inode (not the path, which may name another one).
func (f *sourceFile) intact() bool {
	info, err := f.host.Stat()
	return err == nil && info.Size() == f.info.Size() && info.ModTime().Equal(f.info.ModTime())
}

// useCapture replaces the host file after it changed. The caller holds mu.
func (f *sourceFile) useCapture() error {
	c, err := f.b.capturedFile(f.path, f.node)
	if err != nil {
		return err
	}
	f.host.Close()
	f.host, f.captured = nil, c
	return nil
}

// readAt reads without changing off. The caller holds mu.
func (f *sourceFile) readAt(p []byte, off int64) (int, error) {
	if f.host != nil {
		n, err := f.host.ReadAt(p, off)
		short := err == io.EOF && off < f.info.Size()-int64(n)
		if !short && f.intact() {
			return n, err
		}
		if err := f.useCapture(); err != nil {
			return 0, err
		}
	}
	if f.captured == nil {
		return 0, os.ErrClosed
	}
	return f.captured.ReadAt(p, off)
}

func (f *sourceFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readAt(p, off)
}

func (f *sourceFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.readAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *sourceFile) Seek(off int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.host == nil && f.captured == nil {
		return 0, os.ErrClosed
	}
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += f.off
	case io.SeekEnd:
		off += f.info.Size()
	default:
		return 0, syscall.EINVAL
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	f.off = off
	return off, nil
}

func (f *sourceFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.host != nil {
		err := f.host.Close()
		f.host = nil
		return err
	}
	if f.captured != nil {
		err := f.captured.Close()
		f.captured = nil
		return err
	}
	return os.ErrClosed
}

// copyTo keeps the clone fast path but discards a copy the donor changed
// during cloning, just as readAt discards a read from the changed inode.
func (f *sourceFile) copyTo(dst string, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.host != nil {
		err := clone.FD(f.host, dst, mode)
		if f.intact() {
			return err
		}
		if err == nil {
			if err := os.Remove(dst); err != nil {
				return err
			}
		}
		if err := f.useCapture(); err != nil {
			return err
		}
	}
	if f.captured == nil {
		return os.ErrClosed
	}
	return copyContent(f.captured, dst, mode)
}
