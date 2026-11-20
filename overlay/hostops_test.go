package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestDirTimesStayInIndexUntilSettled checks that memory name changes stamp
// a directory in the index without touching the host, and that Flush and
// Cool write the index's time to the host before anything reads it there.
func TestDirTimesStayInIndexUntilSettled(t *testing.T) {
	f, err := New(t.TempDir(), nil, nil, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("d", 0o755))
	must(t, f.Chtimes("d", past, past))
	write(t, f, "d/new", "data", os.O_CREATE|os.O_WRONLY)
	info, err := f.Lstat("d")
	must(t, err)
	if !info.ModTime().After(past) {
		t.Fatalf("create left the directory time at %v", info.ModTime())
	}
	host, err := os.Lstat(filepath.Join(f.upper, "d"))
	must(t, err)
	if retimable() && !host.ModTime().Equal(past) {
		t.Fatalf("a memory create stamped the host directory: %v", host.ModTime())
	}
	must(t, f.Flush())
	host, err = os.Lstat(filepath.Join(f.upper, "d"))
	must(t, err)
	if !host.ModTime().Equal(info.ModTime()) {
		t.Fatalf("flush left the host directory at %v, index %v", host.ModTime(), info.ModTime())
	}

	must(t, f.Rename("d/new", "d/renamed"))
	info, err = f.Lstat("d")
	must(t, err)
	must(t, f.Settle())
	f.Cool(time.Now().Add(time.Hour))
	if f.index != nil {
		t.Fatal("index stayed loaded")
	}
	cold, err := f.Lstat("d")
	must(t, err)
	if !cold.ModTime().Equal(info.ModTime()) {
		t.Fatalf("cooling moved the directory time from %v to %v", info.ModTime(), cold.ModTime())
	}
}

// retimable reports whether index entries carry their own directory times on this platform.
func retimable() bool {
	info, err := os.Lstat(os.TempDir())
	if err != nil {
		return false
	}
	_, ok := retimed(info, past, past)
	return ok
}

// TestHostOpsConcurrent runs workers that create, spill, read, rename, link
// and remove files in their own directories, on disk or in memory, while the
// writeback worker moves idle files to disk and another worker renames a
// directory whose files spill meanwhile. Every file must read back as written, and after Flush the
// index must match the upper on disk, directory times included.
func TestHostOpsConcurrent(t *testing.T) {
	f, err := New(t.TempDir(), nil, nil, NewMemory(1<<20, 64<<10, time.Millisecond))
	must(t, err)
	const workers, rounds = 4, 150
	for w := range workers {
		must(t, f.MkdirAll(fmt.Sprintf("w%d", w), 0o755))
	}
	must(t, f.MkdirAll("r/a", 0o755))
	var wg sync.WaitGroup
	errs := make(chan error, workers+1)
	check := func(p, want string) error {
		h, err := f.Open(p)
		if err != nil {
			return err
		}
		defer h.Close()
		got := make([]byte, len(want)+1)
		n, err := h.Read(got)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if string(got[:n]) != want {
			return fmt.Errorf("%s holds %q, want %q", p, got[:n], want)
		}
		return nil
	}
	for w := range workers {
		wg.Go(func() {
			for i := range rounds {
				p := fmt.Sprintf("w%d/f%d", w, i)
				data := fmt.Sprintf("%d-%d", w, i)
				if err := writeFile(f, p, []byte(data), os.O_CREATE|os.O_EXCL|os.O_WRONLY); err != nil {
					errs <- err
					return
				}
				switch i % 4 {
				case 1:
					time.Sleep(3 * time.Millisecond) // the writeback worker may spill it meanwhile
				case 2:
					if err := f.Flush(); err != nil { // spills everything, so it is renamed on disk
						errs <- err
						return
					}
				}
				if err := check(p, data); err != nil {
					errs <- err
					return
				}
				q := p + ".r"
				if err := f.Rename(p, q); err != nil {
					errs <- err
					return
				}
				if err := check(q, data); err != nil {
					errs <- err
					return
				}
				if i%5 < 2 {
					// A second name, removed again: the file's link count goes up and down.
					if err := f.Link(q, q+".l"); err != nil {
						errs <- err
						return
					}
					if err := check(q+".l", data); err != nil {
						errs <- err
						return
					}
					if err := f.Remove(q + ".l"); err != nil {
						errs <- err
						return
					}
				}
				if i%2 == 0 {
					if err := f.Remove(q); err != nil {
						errs <- err
						return
					}
				}
			}
		})
	}
	wg.Go(func() {
		for i := range rounds {
			dir := []string{"r/a", "r/b"}[i%2]
			if err := writeFile(f, fmt.Sprintf("%s/f%d", dir, i), []byte("r"), os.O_CREATE|os.O_EXCL|os.O_WRONLY); err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Millisecond)
			if err := f.Rename(dir, []string{"r/b", "r/a"}[i%2]); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for w := range workers {
		for i := range rounds {
			p := fmt.Sprintf("w%d/f%d.r", w, i)
			_, err := f.Lstat(p)
			if i%2 == 0 {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("%s: %v, want removed", p, err)
				}
				continue
			}
			must(t, check(p, fmt.Sprintf("%d-%d", w, i)))
		}
	}
	for i := range rounds {
		must(t, check(fmt.Sprintf("r/a/f%d", i), "r"))
	}
	must(t, f.Flush())
	assertIndexMatchesDisk(t, f)
	for p, e := range f.index.entries {
		if e.Kind != KindDir {
			continue
		}
		host, err := os.Lstat(f.up(p))
		must(t, err)
		if !host.ModTime().Equal(e.Info.ModTime()) {
			t.Fatalf("directory %q: host time %v, index %v after flush", p, host.ModTime(), e.Info.ModTime())
		}
	}
}
