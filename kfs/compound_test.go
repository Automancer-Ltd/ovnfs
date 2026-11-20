//go:build !windows

package kfs

import (
	"bytes"
	"errors"
	"testing"
)

// readBody encodes a READ body of n bytes at off.
func readBody(off uint64, n uint32) []byte {
	return le.AppendUint32(le.AppendUint32(le.AppendUint64(nil, off), n), 0)
}

// writeBody encodes a WRITE body with its data.
func writeBody(off uint64, data []byte) []byte {
	return append(le.AppendUint32(le.AppendUint32(le.AppendUint64(nil, off), uint32(len(data))), 0), data...)
}

// TestCompound checks that sub-requests run in order as if alone: a failing
// one is answered and the next still runs, and a READ sees the WRITE before it.
func TestCompound(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	lib := must(c.Lookup(must(c.Lookup(RootID, "pkg"))(t).ID, "lib.go"))(t)
	subs := must(c.Compound(
		c.Request(OpLookup, RootID, []byte("missing")),
		c.Request(OpWrite, lib.ID, writeBody(0, []byte("PACKAGE"))),
		c.Request(OpRead, lib.ID, readBody(0, 64)),
		c.Request(OpGetattr, lib.ID),
	))(t)
	if len(subs) != 4 {
		t.Fatalf("%d replies, want 4", len(subs))
	}
	if !errors.Is(subs[0].Err, ENOENT) {
		t.Fatalf("lookup of a missing name: %v", subs[0].Err)
	}
	if subs[1].Err != nil || subs[2].Err != nil || subs[3].Err != nil {
		t.Fatalf("replies after the failure: %v %v %v", subs[1].Err, subs[2].Err, subs[3].Err)
	}
	if got := string(subs[2].Result); got != "PACKAGE pkg\n" {
		t.Fatalf("read after write in one compound = %q", got)
	}
	var a Attr
	if err := a.decode(subs[3].Result); err != nil || a.ID != lib.ID || a.Size != 12 {
		t.Fatalf("getattr = %+v, %v", a, err)
	}
	if subs[0].Epoch != c.Epoch || subs[3].Epoch != c.Epoch {
		t.Fatalf("sub-reply epochs %x %x, outer %x", subs[0].Epoch, subs[3].Epoch, c.Epoch)
	}

	ro := dial(t, f.serve(t, "vm", false))
	subs = must(ro.Compound(ro.Request(OpWrite, lib.ID, writeBody(0, []byte("x"))), ro.Request(OpGetattr, lib.ID)))(t)
	if !errors.Is(subs[0].Err, EROFS) || subs[1].Err != nil {
		t.Fatalf("read-only compound: %v, %v", subs[0].Err, subs[1].Err)
	}
}

// TestCompoundMalformed checks that a nested or truncated compound fails
// whole and runs none of its sub-requests.
func TestCompoundMalformed(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	lib := must(c.Lookup(must(c.Lookup(RootID, "pkg"))(t).ID, "lib.go"))(t)
	write := c.Request(OpWrite, lib.ID, writeBody(0, []byte("CHANGED")))
	nested := c.Request(OpCompound, 0, le.AppendUint32(nil, uint32(len(write))), write)
	if _, err := c.Compound(write, nested); !errors.Is(err, EINVAL) {
		t.Fatalf("nested compound: %v, want EINVAL", err)
	}
	cut := append(le.AppendUint32(nil, uint32(len(write))), write...)
	cut = append(cut, le.AppendUint32(nil, 100)...)
	cut = append(cut, write[:10]...)
	if _, err := c.Call(OpCompound, 0, cut); !errors.Is(err, EINVAL) {
		t.Fatalf("truncated compound: %v, want EINVAL", err)
	}
	short := append(le.AppendUint32(nil, 3), 1, 2, 3)
	if _, err := c.Call(OpCompound, 0, short); !errors.Is(err, EINVAL) {
		t.Fatalf("sub-request shorter than a header: %v, want EINVAL", err)
	}
	if got := must(c.Read(lib.ID, 0, 64))(t); string(got) != "package pkg\n" {
		t.Fatalf("a malformed compound ran a write: %q", got)
	}
	if subs := must(c.Compound())(t); len(subs) != 0 {
		t.Fatalf("empty compound: %d replies", len(subs))
	}
}

// TestCompoundReplyLimit checks that a sub-request whose reply could
// overflow the reply frame is refused alone with EMSGSIZE, while the others
// run.
func TestCompoundReplyLimit(t *testing.T) {
	f := newFixture(t, "vm")
	c := dial(t, f.serve(t, "vm", true))
	big := must(c.Create(RootID, "big", 0o644, CreateExcl))(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), MaxIO/16)
	must(c.Write(big.ID, 0, data))(t)
	subs := must(c.Compound(
		c.Request(OpRead, big.ID, readBody(0, MaxIO)),
		c.Request(OpRead, big.ID, readBody(0, MaxIO)),
		c.Request(OpGetattr, big.ID),
	))(t)
	if subs[0].Err != nil || !bytes.Equal(subs[0].Result, data) {
		t.Fatalf("first read: %v, %d bytes", subs[0].Err, len(subs[0].Result))
	}
	if !errors.Is(subs[1].Err, EMSGSIZE) || len(subs[1].Result) != 0 {
		t.Fatalf("read past the frame: %v, %d bytes", subs[1].Err, len(subs[1].Result))
	}
	if subs[2].Err != nil {
		t.Fatalf("getattr after the refused read: %v", subs[2].Err)
	}
}
