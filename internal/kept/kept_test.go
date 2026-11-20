package kept

import (
	"os"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/stencil-hq/ovnfs/overlay"
)

// TestKeptEvictionAfterOwnerClose covers cached writers whose checkout was
// removed before cache pressure evicts them. Keep reports eviction close errors
// to the new WRITE, so a second-close error would fail unrelated compiler output.
func TestKeptEvictionAfterOwnerClose(t *testing.T) {
	for _, storage := range []string{"disk", "spilled"} {
		t.Run(storage, func(t *testing.T) {
			var memory *overlay.Memory
			if storage == "spilled" {
				memory = overlay.NewMemory(1<<20, 1<<20, 0)
			}
			fs, err := overlay.New(t.TempDir(), nil, nil, memory)
			if err != nil {
				t.Fatal(err)
			}
			file, err := fs.OpenFile("output", os.O_CREATE|os.O_WRONLY, 0o755)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.Write([]byte("compiler output")); err != nil {
				t.Fatal(err)
			}
			if err := fs.Flush(); err != nil {
				t.Fatal(err)
			}
			cache := Cache[string]{Idle: time.Hour}
			if err := cache.Keep("removed/output", file); err != nil {
				t.Fatal(err)
			}
			fs.CloseFiles()
			cache.mu.Lock()
			evicted := cache.evictLocked()
			cache.mu.Unlock()
			if evicted == nil {
				t.Fatal("idle writer was not evicted")
			}
			if err := evicted.Close(); err != nil {
				t.Fatalf("evicting a retired writer would fail the next WRITE: %v", err)
			}
		})
	}
}

// closeCount is a file that counts its closes.
type closeCount struct {
	billy.File
	closes *int
}

// Close counts the close.
func (c closeCount) Close() error {
	*c.closes++
	return nil
}

// TestDropWhileInUse checks that a file dropped during a call closes once,
// when the call returns, and is no longer kept.
func TestDropWhileInUse(t *testing.T) {
	var cache Cache[int]
	closes := 0
	if err := cache.Keep(1, closeCount{closes: &closes}); err != nil {
		t.Fatal(err)
	}
	ok, err := cache.With(1, func(billy.File) error {
		if err := cache.Drop(1); err != nil {
			t.Fatal(err)
		}
		if closes != 0 {
			t.Fatal("dropped file closed while in use")
		}
		return nil
	})
	if !ok || err != nil || closes != 1 || cache.Has(1) {
		t.Fatalf("after drop: ok %v err %v closes %d kept %v", ok, err, closes, cache.Has(1))
	}
	if err := cache.Drop(1); err != nil || closes != 1 {
		t.Fatalf("second drop: %v, %d closes", err, closes)
	}
}
