package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/trace"
	"testing"
)

// TestMemorySyscallTrace records an execution trace of each memory-tier
// name change (create and write, rename within a directory, unlink), and of
// spilling, opening, renaming and removing the files once on disk, each on
// its own, so `go tool trace -d=parsed` can count the host syscalls each makes
// (GoSyscallBegin events). It runs only with OVERLAY_SYSCALL_TRACE naming a
// directory for the traces; nothing spills, since the budget's idle time is
// zero.
func TestMemorySyscallTrace(t *testing.T) {
	dir := os.Getenv("OVERLAY_SYSCALL_TRACE")
	if dir == "" {
		t.Skip("set OVERLAY_SYSCALL_TRACE to a directory for the traces")
	}
	const n = 100
	f, err := New(t.TempDir(), nil, nil, NewMemory(64<<20, 256<<10, 0))
	must(t, err)
	must(t, f.MkdirAll("d", 0o755))
	data := make([]byte, 1024)
	phase := func(name string, op func(i int) error) {
		out, err := os.Create(filepath.Join(dir, name+".trace"))
		must(t, err)
		must(t, trace.Start(out))
		for i := range n {
			if err := op(i); err != nil {
				trace.Stop()
				t.Fatalf("%s %d: %v", name, i, err)
			}
		}
		trace.Stop()
		must(t, out.Close())
	}
	name := func(i int, suffix string) string { return fmt.Sprintf("d/f%03d%s", i, suffix) }
	phase("create", func(i int) error {
		h, err := f.OpenFile(name(i, ""), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := h.Write(data); err != nil {
			return err
		}
		return h.Close()
	})
	phase("stat", func(i int) error { _, err := f.Lstat(name(i, "")); return err })
	phase("rename", func(i int) error { return f.Rename(name(i, ""), name(i, ".r")) })
	phase("unlink", func(i int) error { return f.Remove(name(i, ".r")) })
	if len(f.files) != 0 {
		t.Fatalf("%d memory files left", len(f.files))
	}
	// The same on disk: files spilled by Flush, then opened, renamed and removed.
	for i := range n {
		h, err := f.OpenFile(name(i, ""), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		must(t, err)
		_, err = h.Write(data)
		must(t, err)
		must(t, h.Close())
	}
	phase("flush", func(i int) error {
		if i == 0 {
			return f.Flush()
		}
		return nil
	})
	phase("hopen", func(i int) error {
		h, err := f.Open(name(i, ""))
		if err != nil {
			return err
		}
		return h.Close()
	})
	phase("hrename", func(i int) error { return f.Rename(name(i, ""), name(i, ".r")) })
	phase("hunlink", func(i int) error { return f.Remove(name(i, ".r")) })
}
