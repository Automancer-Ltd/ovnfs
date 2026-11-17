package nfs

import (
	"os"
	"path"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-billy/v5"
)

// allocationFS determines no id for "f", as a backend does for an entry
// whose identity it cannot read, and ids for everything else.
type allocationFS struct{ *testFS }

func (f allocationFS) FileID(p string) uint64 {
	if p == "f" {
		return 0
	}
	return f.testFS.FileID(p)
}
func (f allocationFS) IDOf(p string, _ os.FileInfo) uint64 { return f.FileID(p) }
func (f allocationFS) ChildID(dir string, info os.FileInfo) uint64 {
	return f.FileID(path.Join(dir, info.Name()))
}

// TestRefusedAllocationIsIO answers IO rather than a handle of id 0 when the
// backend cannot identify the entry looked up.
func TestRefusedAllocationIsIO(t *testing.T) {
	fs := allocationFS{newTestFS(t)}
	fs.write(t, "f", "content")
	c := v3server(t, fs)
	if st, _ := c.v3call(v3Lookup, func(e *encoder) { dirop(e, 1, "f") }); st != stIO {
		t.Fatalf("LOOKUP %d", st)
	}
	_, addr := v4serve(t, fs, nil)
	v4 := v4dial(t, addr)
	if st := v4.run(v4Root(), v4Look("f"), v4GetFH()).status; st != stIO {
		t.Fatalf("LOOKUP %d", st)
	}
}

// refusedSpecialFS commits the creation but has no id for its result.
type refusedSpecialFS struct{ allocationFS }

func (f refusedSpecialFS) Mknod(name string, mode os.FileMode) error {
	file, err := f.testFS.Create(name)
	if err != nil {
		return err
	}
	return file.Close()
}

// TestSpecialCreationRefusedIdentity answers IO, with a well-formed failure
// arm, for a MKNOD or CREATE whose new file has no id.
func TestSpecialCreationRefusedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  fileType
	}{{"socket", typeSock}, {"fifo", typeFifo}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("v3", func(t *testing.T) {
				fs := refusedSpecialFS{allocationFS{newTestFS(t)}}
				c := v3server(t, fs)
				st, d := c.v3call(v3Mknod, func(e *encoder) {
					dirop(e, 1, "f")
					e.u32(uint32(tc.typ))
					noSattr(e)
				})
				if st != stIO {
					t.Fatalf("MKNOD status %d, want IO", st)
				}
				// MKNOD3resfail contains wcc_data: absent pre-op and
				// post-op attributes, with no success-arm bytes left over.
				if d.bool() || d.bool() || d.err != nil || len(d.buf) != 0 {
					t.Fatal("malformed MKNOD failure arm")
				}
				if _, err := fs.Lstat("f"); err != nil {
					t.Fatal("backend creation did not commit:", err)
				}
			})
			t.Run("v4", func(t *testing.T) {
				fs := refusedSpecialFS{allocationFS{newTestFS(t)}}
				_, addr := v4serve(t, fs, nil)
				c := v4dial(t, addr)
				r := c.run(v4Root(), v4Create(tc.typ, "f", "", v4fattr{}), v4GetFH())
				r.skip(op4PutRootFH)
				r.next(op4Create, stIO)
				r.end()
				if _, err := fs.Lstat("f"); err != nil {
					t.Fatal("backend creation did not commit:", err)
				}
			})
		})
	}
}

// retainedFaultFile counts the closes of a file the server may keep open.
type retainedFaultFile struct {
	billy.File
	closes atomic.Int64
}

func (f *retainedFaultFile) Close() error { f.closes.Add(1); return f.File.Close() }

// refusedCreateFS has no id for the file "readonly" and records the file it
// opens for it.
type refusedCreateFS struct {
	*testFS
	file *retainedFaultFile
}

func (f *refusedCreateFS) FileID(p string) uint64 {
	if p == "readonly" {
		return 0
	}
	return f.testFS.FileID(p)
}
func (f *refusedCreateFS) IDOf(p string, _ os.FileInfo) uint64 { return f.FileID(p) }
func (f *refusedCreateFS) ChildID(dir string, info os.FileInfo) uint64 {
	return f.FileID(path.Join(dir, info.Name()))
}

func (f *refusedCreateFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err != nil {
		return nil, err
	}
	f.file = &retainedFaultFile{File: file}
	return f.file, nil
}

// TestRefusedCreateOpenClosesRetainedFile answers IO for an OPEN creating a
// file with no id, closing the descriptor kept for it and publishing no
// state.
func TestRefusedCreateOpenClosesRetainedFile(t *testing.T) {
	fs := &refusedCreateFS{testFS: newTestFS(t)}
	srv, addr := v4serve(t, fs, nil)
	c := v4dial(t, addr)
	c.setup(srv, "refused-create", nil)
	r := c.run(v4Root(), v4Open(v4openArgs{owner: c.owner("writer"), access: share4AccessAll, create: true, name: "readonly", attrs: v4Mode(0400)}))
	if r.status != stIO {
		t.Fatalf("OPEN: %d", r.status)
	}
	if fs.file == nil || fs.file.closes.Load() != 1 {
		t.Fatal("refused OPEN did not close retained file once")
	}
	srv.v4.mu.Lock()
	defer srv.v4.mu.Unlock()
	if len(srv.v4.holds) != 0 || len(srv.v4.files) != 0 {
		t.Fatal("refused OPEN published state")
	}
}
