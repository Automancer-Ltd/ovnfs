package overlay

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// TestCreateWithChosenID creates files and a directory with identities
// from AllocIDs, in memory and on disk, and checks they keep identity,
// contents and mtime through a spill and a fresh overlay on the same upper,
// and that an identity is refused when it was never handed out, is no
// Synthetic one or names an entry.
func TestCreateWithChosenID(t *testing.T) {
	for _, tc := range []struct {
		name string
		mem  *Memory
	}{{"memory", NewMemory(1<<20, 64<<10, 0)}, {"disk", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			upper := t.TempDir()
			f, err := New(upper, nil, nil, tc.mem)
			must(t, err)
			must(t, f.MkdirAll("d", 0o755))
			first, n, err := f.AllocIDs(8)
			must(t, err)
			if n != 8 || first.Space() != ident.Synthetic {
				t.Fatalf("AllocIDs = %v, %d", first, n)
			}
			id := func(i uint64) ident.ID { v, _ := ident.New(ident.Synthetic, first.Value()+i); return v }
			mtime := time.Unix(1_700_000_000, 123456700) // in 100 ns steps, as NTFS keeps times
			info, err := f.CreateFile("d/a", 0o640, id(0), mtime, []byte("contents"))
			must(t, err)
			if got, _ := ident.Of(info); got != id(0) || info.Size() != 8 || !info.ModTime().Equal(mtime) || info.Mode().Perm() != 0o640 {
				t.Fatalf("created %v %d %v %v", got, info.Size(), info.ModTime(), info.Mode())
			}
			dir, err := f.MkdirID("d/sub", 0o750, id(1), mtime)
			must(t, err)
			if got, _ := ident.Of(dir); got != id(1) || !dir.ModTime().Equal(mtime) || dir.Mode().Perm() != 0o750 {
				t.Fatalf("made directory %v %v %v", got, dir.ModTime(), dir.Mode())
			}
			_, err = f.CreateFile("d/sub/b", 0o644, id(2), mtime, nil)
			must(t, err)

			refused := func(what, p string, id ident.ID) {
				t.Helper()
				if _, err := f.CreateFile(p, 0o644, id, mtime, nil); !errors.Is(err, syscall.EINVAL) {
					t.Fatalf("%s: %v, want EINVAL", what, err)
				}
				if _, err := f.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("%s created %s: %v", what, p, err)
				}
			}
			refused("an identity in use", "d/x", id(0))
			refused("an identity never handed out", "d/x", id(1000))
			state, _ := ident.New(ident.State, 42)
			refused("a non-Synthetic identity", "d/x", state)
			if _, err := f.CreateFile("d/a", 0o644, id(3), mtime, nil); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("create over an existing name: %v", err)
			}
			if got := read(t, f, "d/a"); got != "contents" {
				t.Fatalf("existing file changed: %q", got)
			}
			// The identity a refused create named is still free on the daemon's side.
			_, err = f.CreateFile("d/c", 0o644, id(3), mtime, nil)
			must(t, err)

			ids := map[string]ident.ID{"d/a": id(0), "d/sub": id(1), "d/sub/b": id(2), "d/c": id(3)}
			// A directory's time moves with the names made in it since.
			times := make(map[string]time.Time)
			for p := range ids {
				info, err := f.Lstat(p)
				must(t, err)
				times[p] = info.ModTime()
			}
			if !times["d/a"].Equal(mtime) || !times["d/sub/b"].Equal(mtime) {
				t.Fatalf("files show mtimes %v, %v, want %v", times["d/a"], times["d/sub/b"], mtime)
			}
			must(t, f.Flush())
			g, err := New(upper, nil, nil, nil)
			must(t, err)
			for p, want := range ids {
				info, err := g.Lstat(p)
				must(t, err)
				if got, _ := ident.Of(info); got != want || !info.ModTime().Equal(times[p]) {
					t.Fatalf("after reopening, %s has identity %v (want %v) and mtime %v (want %v)", p, got, want, info.ModTime(), times[p])
				}
			}
			if got := read(t, g, "d/a"); got != "contents" {
				t.Fatalf("after reopening, d/a holds %q", got)
			}
			// Identities handed out before stay claimable and are never handed out again.
			next, _, err := g.AllocIDs(1)
			must(t, err)
			if next.Value() < first.Value()+8 {
				t.Fatalf("reopened allocator handed out %v again (first run %v+8)", next, first)
			}
			_, err = g.CreateFile("d/late", 0o600, id(4), mtime, []byte("x"))
			must(t, err)
			must(t, g.Flush())
		})
	}
}
