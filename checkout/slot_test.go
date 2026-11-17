package checkout

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// slotOf returns checkout name's slot and life, failing the test without one.
func (f *fixture) slotOf(name string) (uint16, uint32) {
	f.t.Helper()
	slot, life, ok := f.reg.Slot(name)
	if !ok || slot == 0 || life == 0 {
		f.t.Fatalf("checkout %s has slot %d, life %d (%v)", name, slot, life, ok)
	}
	if owner, ownerLife, ok := f.reg.SlotOwner(slot); !ok || owner != name || ownerLife != life {
		f.t.Fatalf("slot %d is held by %q, life %d (%v), want %s", slot, owner, ownerLife, ok, name)
	}
	return slot, life
}

// reopen closes the registry and opens it again on the same state.
func (f *fixture) reopen() {
	f.t.Helper()
	if err := f.reg.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.open()
}

// TestSlotsDelayReuseAndPersist keeps a checkout's export slot, which file
// ids carry, across restarts, and does not hand a removed checkout's slot to
// the next one, whose ids would then name the removed checkout's files.
func TestSlotsDelayReuseAndPersist(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "x"})
	slots := map[string]uint16{}
	lives := map[string]uint32{}
	for _, name := range []string{"a", "b", "c"} {
		f.newCheckout(dir, name)
		slots[name], lives[name] = f.slotOf(name)
	}
	if slots["a"] == slots["b"] || slots["b"] == slots["c"] || slots["a"] == slots["c"] {
		t.Fatalf("checkouts share slots: %v", slots)
	}
	if err := f.reg.Rm(context.Background(), api.RmArgs{Checkout: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := f.reg.SlotOwner(slots["b"]); ok {
		t.Fatal("a removed checkout still holds its slot")
	}
	f.newCheckout(dir, "d")
	slots["d"], lives["d"] = f.slotOf("d")
	if slots["d"] == slots["b"] {
		t.Fatal("the next checkout took the slot just freed")
	}

	f.reopen()
	for _, name := range []string{"a", "c", "d"} {
		if slot, life := f.slotOf(name); slot != slots[name] || life != lives[name] {
			t.Errorf("%s had slot %d life %d, after reopening %d and %d", name, slots[name], lives[name], slot, life)
		}
	}
	f.newCheckout(dir, "e")
	if slot, life := f.slotOf("e"); slot == slots["b"] || life <= lives["d"] {
		t.Fatalf("after reopening, a new checkout took slot %d life %d", slot, life)
	}
}

// TestUnslottedCheckoutsGetSlots gives checkouts persisted without a slot
// distinct ones when the registry opens.
func TestUnslottedCheckoutsGetSlots(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"file": "x"})
	f.newCheckout(dir, "a")
	f.newCheckout(dir, "b")
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(f.state, "state.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	var checkouts []map[string]json.RawMessage
	if err := json.Unmarshal(state["checkouts"], &checkouts); err != nil {
		t.Fatal(err)
	}
	for _, c := range checkouts {
		delete(c, "slot")
		delete(c, "life")
	}
	delete(state, "next_slot")
	delete(state, "next_life")
	if state["checkouts"], err = json.Marshal(checkouts); err != nil {
		t.Fatal(err)
	}
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f.open()
	a, _ := f.slotOf("a")
	b, _ := f.slotOf("b")
	if a == b {
		t.Fatalf("a and b share slot %d", a)
	}
}
