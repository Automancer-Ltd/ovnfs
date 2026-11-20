//go:build !windows

package kfs

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// TestCreateWithGuestIDs creates files and directories with ids from
// ALLOC_IDS through the protocol: the replies show what was asked, the ids
// resolve, refused ids create nothing, and a COMPOUND sees objects created
// earlier in it.
func TestCreateWithGuestIDs(t *testing.T) {
	f := newFixture(t, "vm", "other")
	c := dial(t, f.serve(t, "vm", true))
	o := dial(t, f.serve(t, "other", true))
	first, n, err := c.AllocIDs(64)
	if err != nil || n == 0 {
		t.Fatalf("ALLOC_IDS = %d, %d, %v", first, n, err)
	}
	if n < 16 {
		t.Fatalf("ALLOC_IDS handed out %d ids", n)
	}
	mtime := time.Unix(1_700_000_000, 987654321)
	data := bytes.Repeat([]byte("x"), 4096)
	a := must(c.CreateWithID(RootID, "a", 0o640, first, mtime, data))(t)
	if a.ID != first || a.MtimeNs != uint64(mtime.UnixNano()) || a.Size != 4096 || a.Mode != sIFREG|0o640 {
		t.Fatalf("created %+v", a)
	}
	if got := must(c.Getattr(first))(t); got != a {
		t.Fatalf("GETATTR %+v, CREATE %+v", got, a)
	}
	if got := must(c.Lookup(RootID, "a"))(t); got.ID != first {
		t.Fatalf("LOOKUP id %d, want %d", got.ID, first)
	}
	if got := must(c.Read(first, 0, 8192))(t); !bytes.Equal(got, data) {
		t.Fatalf("READ %d bytes", len(got))
	}

	refused := func(what, name string, id uint64) {
		t.Helper()
		if _, err := c.CreateWithID(RootID, name, 0o644, id, mtime, nil); !errors.Is(err, EINVAL) {
			t.Fatalf("%s: %v, want EINVAL", what, err)
		}
		if _, err := c.Lookup(RootID, name); !errors.Is(err, ENOENT) {
			t.Fatalf("%s created %q: %v", what, name, err)
		}
	}
	refused("an id in use", "b", first)
	refused("an id never handed out", "b", first+uint64(n)+1000)
	lib := must(c.Lookup(must(c.Lookup(RootID, "pkg"))(t).ID, "lib.go"))(t)
	refused("a non-Synthetic id", "b", lib.ID)
	otherFirst, _ := must2Alloc(t, o)
	refused("another checkout's id", "b", otherFirst)
	if _, err := c.Call(OpCreate, RootID, c.CreateBody("b", 0o644, 0, first+1, 0, nil)...); !errors.Is(err, EINVAL) {
		t.Fatalf("an id without CREATE_EXCL: %v", err)
	}
	if _, err := c.Call(OpCreate, RootID, c.CreateBody("b", 0o644, CreateExcl, 0, 0, data)...); !errors.Is(err, EINVAL) {
		t.Fatalf("content without an id: %v", err)
	}
	if _, err := c.CreateWithID(RootID, "a", 0o644, first+1, mtime, []byte("other")); !errors.Is(err, EEXIST) {
		t.Fatalf("create over an existing name: %v", err)
	}
	if got := must(c.Read(first, 0, 8192))(t); !bytes.Equal(got, data) {
		t.Fatal("a refused create changed the existing file")
	}

	// A directory and children with ids, then changes of them, in one compound.
	dirID, childID, linkName := first+2, first+3, "c-link"
	mr := MkdirReq{Mode: 0o755, NewID: dirID, MtimeNs: uint64(mtime.UnixNano())}
	setMtime := SetattrReq{Valid: SetMtime, MtimeNs: uint64(mtime.Add(time.Hour).UnixNano())}
	subs := must(c.Compound(
		c.Request(OpMkdir, RootID, mr.append(nil), []byte("d")),
		c.Request(OpCreate, dirID, c.CreateBody("c", 0o644, CreateExcl, childID, uint64(mtime.UnixNano()), []byte("child"))...),
		c.Request(OpWrite, childID, writeBody(5, []byte("+"))),
		c.Request(OpSetattr, childID, setMtime.append(nil)),
		c.Request(OpLink, childID, le.AppendUint64(nil, dirID), []byte(linkName)),
		c.Request(OpRename, dirID, (&RenameReq{NewDir: RootID, OldLen: 1, NewLen: 2}).append(nil), []byte("c"), []byte("c2")),
		c.Request(OpUnlink, dirID, []byte(linkName)),
	))(t)
	for i, s := range subs {
		if s.Err != nil {
			t.Fatalf("sub-request %d: %v", i, s.Err)
		}
	}
	var d Attr
	if err := d.decode(subs[0].Result); err != nil || d.ID != dirID || !d.IsDir() || d.MtimeNs != uint64(mtime.UnixNano()) {
		t.Fatalf("MKDIR with an id = %+v, %v", d, err)
	}
	moved := must(c.Lookup(RootID, "c2"))(t)
	if moved.ID != childID || moved.Size != 6 || moved.MtimeNs != setMtime.MtimeNs || moved.Nlink != 1 {
		t.Fatalf("child after the compound = %+v", moved)
	}
	if got := must(c.Read(childID, 0, 64))(t); string(got) != "child+" {
		t.Fatalf("child holds %q", got)
	}
}

// must2Alloc reserves one id through c.
func must2Alloc(t *testing.T, c *Client) (uint64, uint32) {
	t.Helper()
	first, n, err := c.AllocIDs(1)
	if err != nil {
		t.Fatal(err)
	}
	return first, n
}
