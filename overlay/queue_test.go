package overlay

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// applyNext lets f's applier make one more queued change and waits until it
// has, or until it parked.
func applyNext(t *testing.T, f *FS) {
	t.Helper()
	f.mu.Lock()
	before := f.queued
	*f.applyLimit++
	f.startApplier()
	f.mu.Unlock()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		done := f.queued < before || f.parkedErr != nil
		f.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the applier made no change")
		}
	}
}

// treeStates describes every visible name of f: "dir", or "file" with its
// content, which content, when not nil, gives by the file's identity
// instead of reading it (an open would wait for the held queue).
func treeStates(t *testing.T, f *FS, content map[ident.ID]string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	var walk func(string)
	walk = func(dir string) {
		infos, err := f.ReadDir(dir)
		must(t, err)
		for _, info := range infos {
			p := path.Join(dir, info.Name())
			switch {
			case info.IsDir():
				out[p] = "dir"
				walk(p)
			case content != nil:
				id, _ := ident.Of(info)
				c, ok := content[id]
				if !ok {
					t.Fatalf("%q has identity %v, which no file of the test had", p, id)
				}
				out[p] = "file " + c
			default:
				out[p] = "file " + read(t, f, p)
			}
		}
	}
	walk("")
	return out
}

// copyTree copies the host tree at src to dst: directories, regular files
// and symlinks with their permission bits, hard links and the overlay's
// identity attributes, as a crash leaves the disk.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	copied := make(map[inodeKey]string) // a hard-linked file's first copy
	must(t, filepath.WalkDir(src, func(real string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, real)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		info, err := os.Lstat(real)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := os.MkdirAll(to, info.Mode().Perm()|0o700); err != nil {
				return err
			}
			return copyAttrs(real, to)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(real)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case !info.Mode().IsRegular():
			return fmt.Errorf("copyTree: %s is neither file, directory nor symlink", real)
		}
		if key, ok := hostKey(info); ok && hardLinks(info) > 1 {
			if first, ok := copied[key]; ok {
				return os.Link(first, to)
			}
			copied[key] = to
		}
		data, err := os.ReadFile(real)
		if errors.Is(err, fs.ErrPermission) {
			// The owner may lack read permission on its own file.
			if err = os.Chmod(real, info.Mode().Perm()|0o400); err == nil {
				data, err = os.ReadFile(real)
				err = errors.Join(err, os.Chmod(real, info.Mode().Perm()))
			}
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return err
		}
		if err := copyAttrs(real, to); err != nil {
			return err
		}
		return os.Chmod(to, info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
	}))
	// Directories keep the permissions their walk needed until it ended.
	must(t, filepath.WalkDir(src, func(real string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, real)
		if err != nil {
			return err
		}
		info, err := os.Lstat(real)
		if err != nil {
			return err
		}
		return os.Chmod(filepath.Join(dst, rel), info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
	}))
}

// TestQueueCrashLeavesAnEarlierState queues renames, links, unlinks, mkdirs
// and rmdirs without letting the applier make them, then lets it make one
// change at a time. After each, an overlay opened afresh on a copy of the
// upper as it is on disk, as after a crash, must load and show every name
// in a state it had at some point of the sequence.
func TestQueueCrashLeavesAnEarlierState(t *testing.T) {
	upper := t.TempDir()
	f, err := New(upper, nil, nil, NewMemory(1<<20, 64<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("d1", 0o755))
	must(t, f.MkdirAll("d2", 0o755))
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		write(t, f, "d1/"+n, "content of "+n, os.O_CREATE|os.O_WRONLY)
	}
	must(t, f.Flush())
	if !f.queueable() {
		t.Skip("this host makes every change at once")
	}
	content := make(map[ident.ID]string)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		info, err := f.Lstat("d1/" + n)
		must(t, err)
		id, _ := ident.Of(info)
		content[id] = read(t, f, "d1/"+n)
	}
	history := make(map[string]map[string]bool)
	record := func() {
		states := treeStates(t, f, content)
		for p, s := range states {
			if history[p] == nil {
				history[p] = map[string]bool{"absent": true}
			}
			history[p][s] = true
		}
		for p := range history {
			if _, ok := states[p]; !ok {
				history[p]["absent"] = true
			}
		}
	}
	f.mu.Lock()
	limit := 0
	f.applyLimit = &limit
	f.mu.Unlock()
	record()
	// Names that existed before any change were visible from the start.
	for _, h := range history {
		delete(h, "absent")
	}
	steps := []func() error{
		func() error { return f.Rename("d1/a", "d1/a2") },
		func() error { return f.Link("d1/b", "d2/b-link") },
		func() error { return f.Remove("d1/c") },
		func() error { return f.MkdirAll("d3/x", 0o755) },
		func() error { return f.Rename("d1/d", "d3/x/d") },
		func() error { return f.Remove("d2/b-link") },
		func() error { return f.Rename("d1/a2", "d1/b") },
		func() error { return f.Remove("d3/x/d") },
		func() error { return f.Remove("d3/x") },
		func() error { return f.Link("d1/e", "d1/e2") },
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		record()
	}
	f.mu.Lock()
	queued := f.queued
	f.mu.Unlock()
	if queued < len(steps) {
		t.Fatalf("only %d changes queued for %d steps", queued, len(steps))
	}
	final := treeStates(t, f, content)
	for applied := 0; ; applied++ {
		copied := filepath.Join(t.TempDir(), "upper")
		copyTree(t, upper, copied)
		fresh, err := New(copied, nil, nil, nil)
		if err != nil {
			t.Fatalf("after %d of %d changes the upper does not load: %v", applied, queued, err)
		}
		states := treeStates(t, fresh, nil)
		for _, p := range slices.Sorted(maps.Keys(history)) {
			s, ok := states[p]
			if !ok {
				s = "absent"
			}
			if !history[p][s] {
				t.Fatalf("after %d of %d changes %q is %q, a state it never had (%v)", applied, queued, p, s, slices.Sorted(maps.Keys(history[p])))
			}
		}
		for p := range states {
			if history[p] == nil {
				t.Fatalf("after %d of %d changes %q exists, which no state had", applied, queued, p)
			}
		}
		if applied == queued {
			if !maps.Equal(states, final) {
				t.Fatalf("all changes made, disk shows %v, index %v", states, final)
			}
			break
		}
		applyNext(t, f)
	}
	f.mu.Lock()
	f.applyLimit = nil
	f.mu.Unlock()
	must(t, f.Flush())
	assertIndexMatchesDisk(t, f)
}

// TestQueueParksAFailure checks that a queued change the host refuses parks
// the queue, which the budget reports and Flush returns, while new changes
// go to the host themselves; once the host accepts it the queue moves on.
func TestQueueParksAFailure(t *testing.T) {
	if root {
		t.Skip("root ignores the directory permission that makes the unlink fail")
	}
	mem := NewMemory(1<<20, 64<<10, 0)
	f, err := New(t.TempDir(), nil, nil, mem)
	must(t, err)
	must(t, f.MkdirAll("ro", 0o755))
	must(t, f.MkdirAll("rw", 0o755))
	write(t, f, "ro/a", "a", os.O_CREATE|os.O_WRONLY)
	write(t, f, "rw/b", "b", os.O_CREATE|os.O_WRONLY)
	must(t, f.Flush())
	if !f.queueable() {
		t.Skip("this host makes every change at once")
	}
	must(t, os.Chmod(filepath.Join(f.upper, "ro"), 0o555))
	t.Cleanup(func() { os.Chmod(filepath.Join(f.upper, "ro"), 0o755) })
	must(t, f.Remove("ro/a"))
	deadline := time.Now().Add(5 * time.Second)
	for n, _ := mem.QueuesParked(); n == 0; n, _ = mem.QueuesParked() {
		if time.Now().After(deadline) {
			t.Fatal("the refused unlink did not park the queue")
		}
		time.Sleep(time.Millisecond)
	}
	if err := f.Flush(); err == nil {
		t.Fatal("Flush succeeded with the queue parked")
	}
	// A new change on another name goes to the host at once.
	must(t, f.Rename("rw/b", "rw/c"))
	if _, err := os.Lstat(filepath.Join(f.upper, "rw", "c")); err != nil {
		t.Fatalf("a change while parked did not reach the host: %v", err)
	}
	must(t, os.Chmod(filepath.Join(f.upper, "ro"), 0o755))
	for n, _ := mem.QueuesParked(); n != 0; n, _ = mem.QueuesParked() {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("the queue stayed parked after the host accepted the change")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(t, f.Flush())
	if _, err := os.Lstat(filepath.Join(f.upper, "ro", "a")); !os.IsNotExist(err) {
		t.Fatalf("the parked unlink was not made: %v", err)
	}
	assertIndexMatchesDisk(t, f)
}

// holdApplier stops f's applier after the change it is making, waits for
// every change made outside f.mu to finish, and returns with f.mu held, so
// the upper on disk stands still; release lets the applier go on.
func holdApplier(f *FS) (release func()) {
	f.mu.Lock()
	zero := 0
	f.applyLimit = &zero
	for f.appliers > 0 {
		f.mu.Unlock()
		time.Sleep(100 * time.Microsecond)
		f.mu.Lock()
	}
	for op := f.busySync(""); op != nil; op = f.busySync("") {
		f.waitOp(op)
	}
	return func() {
		f.applyLimit = nil
		f.startApplier()
		f.mu.Unlock()
	}
}

// copyAttrs copies the overlay's identity attributes of host entry from to
// entry to.
func copyAttrs(from, to string) error {
	for _, a := range []string{idAttr, nextAttr} {
		v, err := readAttr(from, a)
		if errors.Is(err, errNoAttr) || err == nil && v == 0 {
			continue
		}
		if err != nil {
			return err
		}
		if err := writeAttr(to, a, v); err != nil {
			return err
		}
	}
	return nil
}

// TestCallsReturnAParkedFailure parks a queued unlink and checks that the
// calls that wait for the changes on its name return the failure at once
// instead of waiting for the host to accept it, and succeed once it does.
func TestCallsReturnAParkedFailure(t *testing.T) {
	if root {
		t.Skip("root ignores the directory permission that makes the unlink fail")
	}
	mem := NewMemory(1<<20, 64<<10, 0)
	f, err := New(t.TempDir(), nil, nil, mem)
	must(t, err)
	must(t, f.MkdirAll("ro", 0o755))
	write(t, f, "ro/a", "a", os.O_CREATE|os.O_WRONLY)
	must(t, f.Flush())
	if !f.queueable() {
		t.Skip("this host makes every change at once")
	}
	must(t, os.Chmod(filepath.Join(f.upper, "ro"), 0o555))
	t.Cleanup(func() { os.Chmod(filepath.Join(f.upper, "ro"), 0o755) })
	must(t, f.Remove("ro/a"))
	deadline := time.Now().Add(5 * time.Second)
	for n, _ := mem.QueuesParked(); n == 0; n, _ = mem.QueuesParked() {
		if time.Now().After(deadline) {
			t.Fatal("the refused unlink did not park the queue")
		}
		time.Sleep(time.Millisecond)
	}
	calls := []struct {
		name string
		call func() error
	}{
		{"symlink", func() error { return f.Symlink("t", "ro/a") }},
		{"mknod", func() error { return f.Mknod("ro/a", os.ModeNamedPipe|0o644) }},
		{"mkdir", func() error { return f.MkdirAll("ro/a", 0o755) }},
		{"rename of the directory", func() error { return f.Rename("ro", "moved") }},
		{"chown of the directory", func() error { return f.Chown("ro", os.Getuid(), os.Getgid()) }},
	}
	for _, c := range calls {
		done := make(chan error, 1)
		go func() { done <- c.call() }()
		select {
		case err := <-done:
			if !errors.Is(err, fs.ErrPermission) {
				t.Errorf("%s while the unlink is parked: %v, want the unlink's failure", c.name, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s waits for the parked unlink", c.name)
		}
	}
	must(t, os.Chmod(filepath.Join(f.upper, "ro"), 0o755))
	for n, _ := mem.QueuesParked(); n != 0; n, _ = mem.QueuesParked() {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("the queue stayed parked after the host accepted the change")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must(t, f.Chown("ro", os.Getuid(), os.Getgid()))
	must(t, f.Symlink("t", "ro/a"))
	must(t, f.Rename("ro", "moved"))
	must(t, f.Flush())
	assertIndexMatchesDisk(t, f)
}
