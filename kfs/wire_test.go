package kfs

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// layout writes value as a little-endian integer of size bytes at off of b,
// as the header's offsets say each field lies.
func layout(b []byte, off, size int, value uint64) {
	for i := range size {
		b[off+i] = byte(value >> (8 * i))
	}
}

// TestWireLayout pins the byte layout of ovnfs_req, ovnfs_rep, ovnfs_attr and
// a dirent to the offsets and sizes kmod/ovnfs_wire.h gives them.
func TestWireLayout(t *testing.T) {
	sizes := map[string][2]int{
		"ovnfs_req":         {ReqSize, 24},
		"ovnfs_rep":         {RepSize, 16},
		"ovnfs_attr":        {AttrSize, 64},
		"ovnfs_hello_req":   {HelloReqSize, 8},
		"ovnfs_hello_rep":   {HelloRepSize, 80},
		"ovnfs_statfs_rep":  {StatfsRepSize, 48},
		"ovnfs_setattr_req": {SetattrReqSize, 32},
		"ovnfs_readdir_req": {ReaddirReqSize, 8},
		"ovnfs_readdir_rep": {ReaddirRepSize, 8},
		"ovnfs_read_req":    {ReadReqSize, 16},
		"ovnfs_write_req":   {WriteReqSize, 16},
		"ovnfs_create_req":  {CreateReqSize, 32},
		"ovnfs_mkdir_req":   {MkdirReqSize, 24},
		"ovnfs_alloc_req":   {AllocReqSize, 8},
		"ovnfs_alloc_rep":   {AllocRepSize, 16},
		"ovnfs_mknod_req":   {MknodReqSize, 8},
		"ovnfs_symlink_req": {SymlinkReqSize, 8},
		"ovnfs_link_req":    {LinkReqSize, 8},
		"ovnfs_rename_req":  {RenameReqSize, 16},
		"dirent name":       {DirentHeader, 66},
	}
	for name, s := range sizes {
		if s[0] != s[1] {
			t.Errorf("sizeof %s = %d, header says %d", name, s[0], s[1])
		}
	}
	cr := CreateReq{Mode: 0o644, Flags: CreateExcl, NewID: 0x0102030405060708, MtimeNs: 0x1112131415161718, DataLen: 0x21222324, NameLen: 0x3132}
	wantCreate := make([]byte, 32)
	layout(wantCreate, 0, 4, 0o644)
	layout(wantCreate, 4, 4, uint64(CreateExcl))
	layout(wantCreate, 8, 8, 0x0102030405060708)
	layout(wantCreate, 16, 8, 0x1112131415161718)
	layout(wantCreate, 24, 4, 0x21222324)
	layout(wantCreate, 28, 2, 0x3132)
	if got := cr.append(nil); !bytes.Equal(got, wantCreate) {
		t.Errorf("ovnfs_create_req\n got %x\nwant %x", got, wantCreate)
	}
	mr := MkdirReq{Mode: 0o755, NewID: 0x0102030405060708, MtimeNs: 0x1112131415161718}
	wantMkdir := make([]byte, 24)
	layout(wantMkdir, 0, 4, 0o755)
	layout(wantMkdir, 8, 8, 0x0102030405060708)
	layout(wantMkdir, 16, 8, 0x1112131415161718)
	if got := mr.append(nil); !bytes.Equal(got, wantMkdir) {
		t.Errorf("ovnfs_mkdir_req\n got %x\nwant %x", got, wantMkdir)
	}
	if OpRename != 19 || OpCompound != 20 || OpAllocIDs != 21 {
		t.Errorf("ops: rename %d, compound %d", OpRename, OpCompound)
	}
	if MaxIO != 1<<20 || MaxFrame != 1<<20+4096 || RootID != 1 || NameMax != 255 {
		t.Errorf("limits %d %d %d %d", MaxIO, MaxFrame, RootID, NameMax)
	}

	req := Req{Op: OpRename, UID: 0x11223344, GID: 0x55667788, ID: 0x0102030405060708}
	want := make([]byte, 24)
	layout(want, 0, 2, 19) // op
	layout(want, 4, 4, 0x11223344)
	layout(want, 8, 4, 0x55667788)
	layout(want, 16, 8, 0x0102030405060708)
	if got := req.append(nil); !bytes.Equal(got, want) {
		t.Errorf("ovnfs_req\n got %x\nwant %x", got, want)
	}

	rep := Rep{Error: 116, Epoch: 0xa1a2a3a4a5a6a7a8}
	want = make([]byte, 16)
	layout(want, 0, 4, 116)
	layout(want, 8, 8, 0xa1a2a3a4a5a6a7a8)
	got := make([]byte, RepSize)
	rep.put(got)
	if !bytes.Equal(got, want) {
		t.Errorf("ovnfs_rep\n got %x\nwant %x", got, want)
	}

	attr := Attr{ID: 0x1011, Size: 0x2021, Blocks: 0x3031, MtimeNs: 0x4041, CtimeNs: 0x5051, Mode: 0o100644, Nlink: 0x61, UID: 0x71, GID: 0x81, Rdev: 0x91}
	wantAttr := make([]byte, 64)
	for _, f := range []struct{ off, size int }{{0, 8}, {8, 8}, {16, 8}, {24, 8}, {32, 8}, {40, 4}, {44, 4}, {48, 4}, {52, 4}, {56, 4}} {
		layout(wantAttr, f.off, f.size, map[int]uint64{0: 0x1011, 8: 0x2021, 16: 0x3031, 24: 0x4041, 32: 0x5051, 40: 0o100644, 44: 0x61, 48: 0x71, 52: 0x81, 56: 0x91}[f.off])
	}
	if got := attr.append(nil); !bytes.Equal(got, wantAttr) {
		t.Errorf("ovnfs_attr\n got %x\nwant %x", got, wantAttr)
	}

	d := Dirent{Attr: attr, Name: "abc"}
	wantDirent := make([]byte, 72) // 66 + 3 bytes of name, padded to 8
	copy(wantDirent, wantAttr)
	layout(wantDirent, 64, 2, 3)
	copy(wantDirent[66:], "abc")
	if got := d.append(nil); !bytes.Equal(got, wantDirent) {
		t.Errorf("dirent\n got %x\nwant %x", got, wantDirent)
	}
	var back Dirent
	if n, err := back.decode(wantDirent); err != nil || n != 72 || back != d {
		t.Errorf("dirent decode = %+v, %d, %v", back, n, err)
	}
	if direntSize(6) != 72 || direntSize(7) != 80 {
		t.Errorf("dirent padding: %d %d", direntSize(6), direntSize(7))
	}
}

// TestErrnoMapping checks host errors map to Linux numbers whatever the host.
func TestErrnoMapping(t *testing.T) {
	numbers := map[Errno]uint32{
		EPERM: 1, ENOENT: 2, EIO: 5, EACCES: 13, EEXIST: 17, EXDEV: 18, ENOTDIR: 20, EISDIR: 21,
		EINVAL: 22, EFBIG: 27, ENOSPC: 28, EROFS: 30, ENAMETOOLONG: 36, ENOTEMPTY: 39,
		EOPNOTSUPP: 95, ESTALE: 116,
	}
	for e, n := range numbers {
		if uint32(e) != n {
			t.Errorf("%v = %d, Linux has %d", e, uint32(e), n)
		}
	}
	path := func(err error) error { return &fs.PathError{Op: "op", Path: "p", Err: err} }
	for _, c := range []struct {
		err  error
		want Errno
	}{
		{path(syscall.ENOENT), ENOENT},
		{path(fs.ErrNotExist), ENOENT},
		{path(syscall.EEXIST), EEXIST},
		{path(syscall.ENOTDIR), ENOTDIR},
		{path(syscall.EISDIR), EISDIR},
		{path(syscall.ENOTEMPTY), ENOTEMPTY},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV}, EXDEV},
		{path(syscall.EACCES), EACCES},
		{path(syscall.EPERM), EPERM},
		{path(syscall.EROFS), EROFS},
		{path(syscall.EFBIG), EFBIG},
		{path(syscall.ENOSPC), ENOSPC},
		{path(syscall.EINVAL), EINVAL},
		{path(syscall.ESTALE), ESTALE},
		{path(os.ErrClosed), ESTALE},
		{path(syscall.ENAMETOOLONG), ENAMETOOLONG},
		{path(syscall.ENOTSUP), EOPNOTSUPP},
		{path(syscall.EOPNOTSUPP), EOPNOTSUPP},
		{errors.ErrUnsupported, EOPNOTSUPP},
		{path(EROFS), EROFS},
		{errors.New("anything else"), EIO},
	} {
		if got := errnoOf(c.err); got != c.want {
			t.Errorf("errnoOf(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// chunks is a reader that hands out its data a few bytes per Read.
type chunks struct {
	data []byte
	step int
}

// Read returns at most step bytes.
func (c *chunks) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), c.step)], c.data)
	c.data = c.data[n:]
	return n, nil
}

// TestFrames checks frame reading across short reads, frames sent ahead
// of their turn, a frame larger than the buffer, and the stream's end.
func TestFrames(t *testing.T) {
	frame := func(body []byte) []byte { return append(le.AppendUint32(nil, uint32(len(body))), body...) }
	big := bytes.Repeat([]byte{7}, frameStart+100)
	var stream []byte
	for _, b := range [][]byte{[]byte("one"), big, {}, []byte("four")} {
		stream = append(stream, frame(b)...)
	}
	for _, step := range []int{1, 3, 4096, 1 << 30} {
		var f frames
		r := &chunks{data: stream, step: step}
		for _, want := range [][]byte{[]byte("one"), big, {}, []byte("four")} {
			got, err := f.read(r)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("step %d: frame of %d bytes, %v; want %d bytes", step, len(got), err, len(want))
			}
		}
		if _, err := f.read(r); err != io.EOF {
			t.Fatalf("step %d: end = %v, want EOF", step, err)
		}
	}
	var f frames
	if _, err := f.read(&chunks{data: frame([]byte("cut"))[:5], step: 2}); err != io.ErrUnexpectedEOF {
		t.Fatalf("cut frame: %v", err)
	}
	var oversized frames
	if _, err := oversized.read(bytes.NewReader(le.AppendUint32(nil, MaxFrame+1))); err == nil {
		t.Fatal("oversized frame accepted")
	}
}
