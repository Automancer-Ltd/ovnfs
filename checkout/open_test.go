package checkout

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/overlay"
)

// overlayOpener has the signature of openOverlay.
type overlayOpener = func(upper string, layers []*overlay.Layer, bottom overlay.Bottom, mem *overlay.Memory) (*overlay.FS, error)

// replaceOpenOverlay swaps openOverlay for the test's duration.
func replaceOpenOverlay(t *testing.T, fn overlayOpener) {
	t.Helper()
	old := openOverlay
	openOverlay = fn
	t.Cleanup(func() { openOverlay = old })
}

// closedFixture returns a fixture with n checkouts of one folder, each with
// an edited file, and its registry closed.
func closedFixture(t *testing.T, n int) *fixture {
	t.Helper()
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "base"})
	for i := range n {
		name := fmt.Sprintf("c%d", i)
		f.newCheckout(dir, name)
		f.put(name, "a.txt", "edit "+name)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

// uppers returns the upper directory of each persisted checkout, in disk order.
func (f *fixture) uppers() []string {
	f.t.Helper()
	r := &Registry{config: Config{State: f.state}}
	var uppers []string
	for _, disk := range readDisk(f.t, f.state).Checkouts {
		uppers = append(uppers, r.upperPath(disk.dir()))
	}
	return uppers
}

// TestOpenLoadsCheckoutsInParallel checks that Open opens two checkouts at once.
func TestOpenLoadsCheckoutsInParallel(t *testing.T) {
	f := closedFixture(t, 4)
	var inFlight atomic.Int32
	both := make(chan struct{})
	var once sync.Once
	replaceOpenOverlay(t, func(upper string, layers []*overlay.Layer, bottom overlay.Bottom, mem *overlay.Memory) (*overlay.FS, error) {
		if inFlight.Add(1) >= 2 {
			once.Do(func() { close(both) })
		}
		defer inFlight.Add(-1)
		select {
		case <-both:
		case <-time.After(5 * time.Second):
			return nil, errors.New("no second checkout opened at the same time")
		}
		return overlay.New(upper, layers, bottom, mem)
	})
	f.open()
	if got := len(f.ls().Checkouts); got != 4 {
		t.Fatalf("ls lists %d checkouts, want 4", got)
	}
}

// TestOpenReportsTheFirstCheckoutError checks that Open returns the error of
// the first failing checkout in disk order, even when a later one fails first,
// and returns only after every open has ended.
func TestOpenReportsTheFirstCheckoutError(t *testing.T) {
	f := closedFixture(t, 4)
	uppers := f.uppers()
	thirdFailed := make(chan struct{})
	var active atomic.Int32
	replaceOpenOverlay(t, func(upper string, layers []*overlay.Layer, bottom overlay.Bottom, mem *overlay.Memory) (*overlay.FS, error) {
		active.Add(1)
		defer active.Add(-1)
		switch upper {
		case uppers[1]:
			select {
			case <-thirdFailed:
			case <-time.After(5 * time.Second):
			}
			return nil, errors.New("second checkout broken")
		case uppers[2]:
			defer close(thirdFailed)
			return nil, errors.New("third checkout broken")
		}
		return overlay.New(upper, layers, bottom, mem)
	})
	_, err := Open(f.config())
	if err == nil || !strings.Contains(err.Error(), "second checkout broken") {
		t.Fatalf("Open = %v, want the second checkout's error", err)
	}
	if n := active.Load(); n != 0 {
		t.Fatalf("%d checkout opens still running after Open returned", n)
	}
	openOverlay = overlay.New
	f.open() // the failed Open released the state lock
}

// TestOpenRejectsAMissingBase checks that a checkout naming an unknown base
// fails Open and releases the state lock.
func TestOpenRejectsAMissingBase(t *testing.T) {
	f := closedFixture(t, 2)
	d := readDisk(t, f.state)
	d.Checkouts[1].Base = "b_000000000000"
	if err := writeState(f.state, d); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f.config()); err == nil || !strings.Contains(err.Error(), "references missing base") {
		t.Fatalf("Open = %v, want a missing base error", err)
	}
	lock, err := lockState(f.state)
	if err != nil {
		t.Fatalf("state lock held after the failed Open: %v", err)
	}
	lock.Close()
}

// TestOpenRestoresEveryCheckout checks that a reopen keeps each checkout and
// its edits.
func TestOpenRestoresEveryCheckout(t *testing.T) {
	f := closedFixture(t, 3)
	f.open()
	ls := f.ls()
	if len(ls.Checkouts) != 3 {
		t.Fatalf("ls after reopen = %+v", ls.Checkouts)
	}
	for i := range 3 {
		name := fmt.Sprintf("c%d", i)
		if got := f.read(name, "a.txt"); got != "edit "+name {
			t.Fatalf("%s a.txt = %q", name, got)
		}
	}
}
