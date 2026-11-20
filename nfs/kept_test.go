package nfs

import (
	"bytes"
	"os"
	"runtime"
	"sync"
	"testing"

	"github.com/go-git/go-billy/v5"
)

// seekWriteFS exposes only billy's sequential file API to the NFS writer.
type seekWriteFS struct{ *testFS }

// OpenFile hides WriteAt so cached writes must serialize Seek with Write.
func (f *seekWriteFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	return seekWriteFile{File: file}, nil
}

type seekWriteFile struct{ billy.File }

// Seek yields between positioning and writing to expose shared-offset races.
func (f seekWriteFile) Seek(off int64, whence int) (int64, error) {
	pos, err := f.File.Seek(off, whence)
	runtime.Gosched()
	return pos, err
}

func TestV3CachedWritesPreserveOffsets(t *testing.T) {
	fs := &seekWriteFS{newTestFS(t)}
	fs.write(t, "file", "")
	s := &Server{FS: fs}
	t.Cleanup(func() { closeKept(s) })
	id := fs.FileID("file")
	const workers, block = 16, 64
	want := make([]byte, workers*block)
	if _, err := s.writeAt3(id, 0, "file", want, 0, false); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range workers {
		data := bytes.Repeat([]byte{byte(i + 1)}, block)
		copy(want[i*block:], data)
		wg.Go(func() {
			for range 32 {
				n, err := s.writeAt3(id, 0, "file", data, int64(i*block), false)
				if err != nil || n != len(data) {
					t.Errorf("write at %d: %d bytes, %v", i*block, n, err)
					return
				}
			}
		})
	}
	wg.Wait()
	got, err := os.ReadFile(fs.host("file"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("concurrent writes corrupted file offsets: got %x, want %x", got, want)
	}
}
