//go:build !windows

package kfs

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/overlay"
)

// TestOpsByIDAgainstRenames runs operations by id on one connection while
// another renames the file back and forth, renames its directory back and
// forth and renames other files onto its old name, for a file in the memory
// tier and one on disk. No operation by id may fail, none may touch the
// files renamed onto the old name, and the writes must end up in the file.
func TestOpsByIDAgainstRenames(t *testing.T) {
	for _, tc := range []struct {
		name string
		mem  *overlay.Memory
	}{{"memory", overlay.NewMemory(16<<20, 256<<10, 0)}, {"disk", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWith(t, tc.mem, "vm")
			sock := f.serve(t, "vm", true)
			ops, mover := dial(t, sock), dial(t, sock)
			dir := must(mover.Mkdir(RootID, "d", 0o755))(t)
			file := must(mover.Create(dir.ID, "a", 0o644, CreateExcl))(t)
			const size = 64
			want := make([]byte, size)
			must(mover.Write(file.ID, 0, want))(t)
			stamp := time.Unix(1_000_000_000, 0) // what no other file carries

			var wg sync.WaitGroup
			stop := make(chan struct{})
			errs := make(chan error, 2)
			wg.Go(func() {
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					b := byte('a' + i%26)
					want[i%size] = b
					if _, err := ops.Write(file.ID, uint64(i%size), []byte{b}); err != nil {
						errs <- fmt.Errorf("WRITE %d: %w", i, err)
						return
					}
					sa := SetattrReq{Valid: SetMtime, MtimeNs: uint64(stamp.UnixNano())}
					if _, err := ops.Setattr(file.ID, sa); err != nil {
						errs <- fmt.Errorf("SETATTR %d: %w", i, err)
						return
					}
					got, err := ops.Read(file.ID, 0, size)
					if err != nil {
						errs <- fmt.Errorf("READ %d: %w", i, err)
						return
					}
					if !bytes.Equal(got, want) {
						errs <- fmt.Errorf("READ %d returned %q, want %q", i, got, want)
						return
					}
					if _, err := ops.Getattr(file.ID); err != nil {
						errs <- fmt.Errorf("GETATTR %d: %w", i, err)
						return
					}
				}
			})
			wg.Go(func() {
				defer close(stop)
				step := func(err error) bool {
					if err != nil {
						errs <- err
					}
					return err == nil
				}
				for i := range 300 {
					other := fmt.Sprintf("g%d", i)
					if !step(mover.Rename(dir.ID, "a", dir.ID, "b", 0)) {
						return
					}
					g, err := mover.Create(dir.ID, other, 0o644, CreateExcl)
					if !step(err) || !step(mover.Rename(dir.ID, other, dir.ID, "a", 0)) {
						return
					}
					if !step(mover.Rename(RootID, "d", RootID, "e", 0)) || !step(mover.Rename(RootID, "e", RootID, "d", 0)) {
						return
					}
					got, err := mover.Getattr(g.ID)
					if !step(err) {
						return
					}
					if got.Size != 0 || got.MtimeNs == uint64(stamp.UnixNano()) {
						errs <- fmt.Errorf("round %d: the file renamed onto the old name was changed: %+v", i, got)
						return
					}
					if !step(mover.Rename(dir.ID, "b", dir.ID, "a", 0)) {
						return
					}
				}
			})
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			if t.Failed() {
				return
			}
			at := must(ops.Lookup(dir.ID, "a"))(t)
			if at.ID != file.ID || at.MtimeNs != uint64(stamp.UnixNano()) {
				t.Fatalf("d/a is %+v, want file %d with mtime %v", at, file.ID, stamp)
			}
			if got := must(ops.Read(file.ID, 0, size))(t); !bytes.Equal(got, want) {
				t.Fatalf("file holds %q, want %q", got, want)
			}
		})
	}
}
