package overlay

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// TestNamesTheVolumeCannotHoldApart creates, in the memory tier, names the
// upper's volume refuses or takes for an existing entry, and checks that
// each call answers as the host would, that nothing parks and that the
// index and the disk agree after Flush.
func TestNamesTheVolumeCannotHoldApart(t *testing.T) {
	mem := NewMemory(1<<20, 64<<10, 0)
	f, err := New(t.TempDir(), nil, nil, mem)
	must(t, err)
	must(t, f.MkdirAll("d", 0o755))
	rule := f.volume
	t.Logf("volume: %+v", rule)
	create := func(p string) error { return writeFile(f, p, []byte(p), os.O_WRONLY|os.O_CREATE|os.O_EXCL) }
	expect := func(what string, err error, folds bool, want syscall.Errno) {
		t.Helper()
		if folds && !errors.Is(err, want) || !folds && err != nil {
			t.Fatalf("%s: %v (want %v: %v)", what, err, folds, want)
		}
	}

	expect("create of invalid UTF-8", create("d/\xff\xfe"), rule.rejectInvalid, syscall.EILSEQ)
	expect("mkdir of invalid UTF-8", f.MkdirAll("d/\xfe\xff", 0o755), rule.rejectInvalid, syscall.EILSEQ)
	// Each name below is free unless the volume folds it onto an existing one.
	must(t, create("d/\u00e9"))
	expect("create of the NFD form", create("d/e\u0301"), rule.foldNorm, syscall.EEXIST)
	must(t, create("d/\u00fc"))
	expect("symlink at the NFD form", f.Symlink("t", "d/u\u0308"), rule.foldNorm, syscall.EEXIST)
	must(t, create("d/Case"))
	expect("create of the other case", create("d/case"), rule.foldCase, syscall.EEXIST)
	must(t, create("d/x"))
	expect("link at the other case", f.Link("d/x", "d/cASE"), rule.foldCase, syscall.EEXIST)
	expect("rename onto the other case", f.Rename("d/x", "d/CASE"), rule.foldCase, syscall.EEXIST)
	// A file may change the case of its own name.
	must(t, f.Rename("d/Case", "d/CASE"))
	if _, err := f.Lstat("d/CASE"); err != nil {
		t.Fatalf("after a rename to another case: %v", err)
	}
	if n, msg := mem.QueuesParked(); n != 0 {
		t.Fatalf("the queue parked: %s", msg)
	}
	must(t, f.Flush())
	assertIndexMatchesDisk(t, f)
}
