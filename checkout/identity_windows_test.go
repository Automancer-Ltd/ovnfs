package checkout

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// TestOpenRepairsShortIDStreams reopens a checkout whose upper has two
// interrupted identity writes alongside a valid copy-up from the base.
func TestOpenRepairsShortIDStreams(t *testing.T) {
	f := closedFixture(t, 1)
	upper := f.uppers()[0]
	stream := func(name string) string { return filepath.Join(upper, name) + ":ovnfs.id" }
	readID := func(name string) ident.ID {
		t.Helper()
		b, err := os.ReadFile(stream(name))
		if err != nil || len(b) != 8 {
			t.Fatalf("%s stream = %x, %v; want eight bytes", name, b, err)
		}
		return ident.ID(binary.BigEndian.Uint64(b))
	}
	inherited := readID("a.txt")
	if inherited.Space() != ident.Capture {
		t.Fatalf("copy-up has identity %#x; want a base identity", inherited)
	}
	for name, fragment := range map[string]string{"empty": "", "partial": "\xff\xff\xff"} {
		write(t, filepath.Join(upper, name), name)
		write(t, stream(name), fragment)
	}
	f.open()
	if got := len(f.ls().Checkouts); got != 1 {
		t.Fatalf("ls lists %d checkouts, want 1", got)
	}
	ids := map[string]ident.ID{"a.txt": inherited}
	for _, name := range []string{"empty", "partial"} {
		if got := f.read("c0", name); got != name {
			t.Fatalf("%s contents = %q", name, got)
		}
		ids[name] = readID(name)
		if ids[name].Space() != ident.Synthetic {
			t.Fatalf("%s repaired identity = %#x; want synthetic", name, ids[name])
		}
	}
	if ids["empty"] == ids["partial"] {
		t.Fatalf("both recovered files received identity %#x", ids["empty"])
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	for name, want := range ids {
		if got := readID(name); got != want {
			t.Errorf("%s identity after reopen = %#x, want %#x", name, got, want)
		}
	}
}

// A longer stream or an invalid full identity is not an interrupted write;
// a truncated allocator mark must never be mistaken for a fresh counter.
func TestOpenRejectsOtherIDStreamDamage(t *testing.T) {
	for _, tc := range []struct{ name, suffix, data string }{
		{"long ID", `\a.txt:ovnfs.id`, "123456789"},
		{"invalid ID", `\a.txt:ovnfs.id`, "12345678"},
		{"short allocator mark", ":ovnfs.next", "123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := closedFixture(t, 1)
			upper := f.uppers()[0]
			write(t, upper+tc.suffix, tc.data)
			reg, err := Open(f.config())
			if err == nil {
				reg.Close()
				t.Fatal("opened registry with unrecoverable metadata")
			}
			if !strings.Contains(err.Error(), strings.Split(upper+tc.suffix, ":ovnfs.")[0]) {
				t.Fatalf("unexpected Open error: %v", err)
			}
		})
	}
}
