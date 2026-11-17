package export

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/overlay"
)

// newTestExport exports a real registry holding one checkout, "co", of a
// folder with a single file "source".
func newTestExport(t testing.TB) *Export {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	var e *Export
	reg, err := checkout.Open(checkout.Config{
		State:     filepath.Join(dir, "state"),
		Mount:     filepath.Join(dir, "mount"),
		Memory:    overlay.NewMemory(64<<20, 1<<20, 0),
		BlobCache: 64 << 20,
		Retire: func(name string, slot uint16) {
			if e != nil {
				e.Retire(name, slot)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reg.Close(); err != nil {
			t.Errorf("close registry: %v", err)
		}
	})
	e = New(NewRouter(reg), dir)
	if _, err := reg.New(context.Background(), api.NewArgs{Dir: repo, Name: "co"}); err != nil {
		t.Fatal(err)
	}
	return e
}

// newMetadataTestFS returns the export's filesystem, as every view wraps it.
func newMetadataTestFS(t *testing.T) *nodeFS { return newTestExport(t).fs }
