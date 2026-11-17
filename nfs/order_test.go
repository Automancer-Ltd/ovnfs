package nfs

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5"
)

// heldWriteFS delays every write to a regular file and, with release set,
// holds it until release closes, announcing it on entered first.
type heldWriteFS struct {
	*testFS
	delay   time.Duration
	release chan struct{}
	entered chan struct{}
}

// OpenFile wraps writable files so their writes are delayed or held.
func (f *heldWriteFS) OpenFile(p string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.testFS.OpenFile(p, flag, perm)
	if err != nil || flag&(os.O_WRONLY|os.O_RDWR) == 0 {
		return file, err
	}
	return heldFile{File: file, fs: f}, nil
}

// heldFile is a writable file of a heldWriteFS.
type heldFile struct {
	billy.File
	fs *heldWriteFS
}

// WriteAt writes once the file system lets it.
func (f heldFile) WriteAt(b []byte, off int64) (int, error) {
	if f.fs.entered != nil {
		f.fs.entered <- struct{}{}
	}
	if f.fs.release != nil {
		<-f.fs.release
	}
	time.Sleep(f.fs.delay)
	return writeAt(f.File, b, off)
}

// writeArgs encodes WRITE3args writing data at off to file id, FILE_SYNC.
func writeArgs(id, off uint64, data []byte) func(*encoder) {
	return func(e *encoder) {
		e.fh(id, 0)
		e.u64(off)
		e.u32(uint32(len(data)))
		e.u32(fileSync3)
		e.opaque(data)
	}
}

// sizeTime is the size and mtime one side of a wcc_data reports.
type sizeTime struct {
	size  uint64
	mtime [2]uint32
}

// wccOf decodes a WRITE result's wcc_data.
func wccOf(t testing.TB, d *decoder) (pre, post sizeTime) {
	t.Helper()
	if !d.bool() {
		t.Fatal("no pre-op attributes")
	}
	pre.size = d.u64()
	pre.mtime = [2]uint32{d.u32(), d.u32()}
	d.fixed(8) // ctime
	a, ok := postOpOf(t, d)
	if !ok || d.err != nil {
		t.Fatal("no post-op attributes")
	}
	return pre, sizeTime{a.size, a.mtime}
}

// replies reads n successful NFSv3 replies and returns each result, after
// its status, by xid.
func (c *rpcClient) replies(n int) map[uint32]*decoder {
	c.t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.nc.SetReadDeadline(time.Time{}) }()
	got := make(map[uint32]*decoder, n)
	for range n {
		rec, err := readRecord(c.r, nil)
		if err != nil {
			c.t.Fatal(err)
		}
		d := &decoder{buf: rec}
		xid := d.u32()
		if d.u32() != msgReply || d.u32() != replyAccepted {
			c.t.Fatalf("xid %d: call not accepted", xid)
		}
		d.u32()
		d.opaque(maxAuthBytes)
		if stat, st := acceptStat(d.u32()), status(d.u32()); stat != rpcSuccess || st != stOK || d.err != nil {
			c.t.Fatalf("xid %d: accept_stat %d, status %d", xid, stat, st)
		}
		got[xid] = d
	}
	return got
}

// TestV3WritesChainAttributes pipelines appending WRITEs to one file. Each
// reply's pre-op attributes must be the previous reply's post-op ones: the
// macOS client counts a change as its own only then, and otherwise drops the
// pages it just wrote as if another host had changed the file.
func TestV3WritesChainAttributes(t *testing.T) {
	fs := &heldWriteFS{testFS: newTestFS(t), delay: time.Millisecond}
	fs.write(t, "f", "")
	c := dial(t, startServer(t, &Server{FS: fs, Concurrency: 8}))
	id := fs.FileID("f")

	const n = 16
	chunk := []byte("0123456789abcdef")
	for i := range n {
		c.send(uint32(i+1), v3Write, writeArgs(id, uint64(i*len(chunk)), chunk))
	}
	got := c.replies(n)
	var last sizeTime
	for xid := uint32(1); xid <= n; xid++ {
		pre, post := wccOf(t, got[xid])
		if xid > 1 && pre != last {
			t.Fatalf("WRITE %d: pre-op %+v, want the previous post-op %+v", xid, pre, last)
		}
		last = post
	}
	if last.size != n*uint64(len(chunk)) {
		t.Fatalf("final size %d, want %d", last.size, n*len(chunk))
	}
}

// TestV3AttributesWaitForEarlierWrite checks that a GETATTR arriving behind
// a WRITE to the same file is answered after it, with the written
// attributes, while calls on other files are not held up.
func TestV3AttributesWaitForEarlierWrite(t *testing.T) {
	fs := &heldWriteFS{testFS: newTestFS(t), release: make(chan struct{}), entered: make(chan struct{}, 1)}
	fs.write(t, "f", "")
	var once sync.Once
	release := func() { once.Do(func() { close(fs.release) }) }
	t.Cleanup(release)
	c := dial(t, startServer(t, &Server{FS: fs, Concurrency: 4}))
	id := fs.FileID("f")

	c.send(1, v3Write, writeArgs(id, 0, []byte("data")))
	select {
	case <-fs.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("WRITE never reached the backend")
	}
	c.send(2, v3GetAttr, func(e *encoder) { e.fh(id, 0) })
	c.send(3, v3GetAttr, func(e *encoder) { e.fh(1, 0) })
	if xid, ok := c.replyXID(5 * time.Second); !ok || xid != 3 {
		t.Fatalf("first reply %d (%v), want 3: the root does not wait for the WRITE", xid, ok)
	}
	if xid, ok := c.replyXID(200 * time.Millisecond); ok {
		t.Fatalf("reply %d while the WRITE is held", xid)
	}
	release()
	got := c.replies(2)
	_, post := wccOf(t, got[1])
	if a := fattr3Of(got[2]); a.size != post.size || a.mtime != post.mtime {
		t.Fatalf("GETATTR size %d mtime %v, want the WRITE's %+v", a.size, a.mtime, post)
	}
}
