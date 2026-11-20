package kfs

import (
	"fmt"
	"io"
	"net"
	"time"
)

// Client is a synchronous kernel-protocol client over one connection, as
// one VMM port speaks it: one request in flight at a time. Errors the server
// answers are Errno values. It is not safe for concurrent use.
type Client struct {
	conn net.Conn
	in   frames
	req  []byte   // the frame count, ovnfs_req and the body's fixed part
	iov  [][]byte // req and the body parts, written with one writev
	// UID and GID are sent as the caller of every request.
	UID, GID uint32
	// Epoch is the epoch of the last reply.
	Epoch uint64
}

// Dial connects to a kernel-share socket with buffers that hold a whole
// frame, as the VMM sizes its own.
func Dial(sock string) (*Client, error) {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	sizeBuffers(c)
	return NewClient(c), nil
}

// NewClient speaks the protocol over c, which it owns from now on.
func NewClient(c net.Conn) *Client {
	return &Client{conn: c}
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call sends op on file id with body parts after the header, in one write
// that copies none of them, and returns the result, valid until the next
// call; a nonzero reply error is returned as its Errno. Transport and
// framing failures are other errors.
func (c *Client) Call(op Op, id uint64, body ...[]byte) ([]byte, error) {
	req := Req{Op: op, UID: c.UID, GID: c.GID, ID: id}
	c.req = req.append(append(c.req[:0], 0, 0, 0, 0))
	size := len(c.req) - frameHeader
	c.iov = append(c.iov[:0], c.req)
	for _, b := range body {
		size += len(b)
		c.iov = append(c.iov, b)
	}
	if size > MaxFrame {
		return nil, fmt.Errorf("kfs: request of %d bytes exceeds %d", size, MaxFrame)
	}
	le.PutUint32(c.req, uint32(size))
	bufs := net.Buffers(c.iov)
	if _, err := bufs.WriteTo(c.conn); err != nil {
		return nil, err
	}
	frame, err := c.in.read(c.conn)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	var rep Rep
	if err := rep.decode(frame); err != nil {
		return nil, err
	}
	c.Epoch = rep.Epoch
	if rep.Error != 0 {
		return nil, Errno(rep.Error)
	}
	return frame[RepSize:], nil
}

// attrCall runs an op whose result is one ovnfs_attr.
func (c *Client) attrCall(op Op, id uint64, body ...[]byte) (Attr, error) {
	res, err := c.Call(op, id, body...)
	if err != nil {
		return Attr{}, err
	}
	var a Attr
	return a, a.decode(res)
}

// Hello negotiates the protocol version.
func (c *Client) Hello(version uint32) (HelloRep, error) {
	res, err := c.Call(OpHello, 0, le.AppendUint32(le.AppendUint32(nil, version), 0))
	if err != nil {
		return HelloRep{}, err
	}
	var h HelloRep
	return h, h.decode(res)
}

// Poll returns the current epoch.
func (c *Client) Poll() (uint64, error) {
	_, err := c.Call(OpPoll, RootID)
	return c.Epoch, err
}

// Statfs reports the capacity of the tree's volume.
func (c *Client) Statfs() (StatfsRep, error) {
	res, err := c.Call(OpStatfs, RootID)
	if err != nil {
		return StatfsRep{}, err
	}
	var s StatfsRep
	return s, s.decode(res)
}

// Getattr describes file id.
func (c *Client) Getattr(id uint64) (Attr, error) { return c.attrCall(OpGetattr, id) }

// Setattr changes the attributes sa.Valid names.
func (c *Client) Setattr(id uint64, sa SetattrReq) (Attr, error) {
	return c.attrCall(OpSetattr, id, sa.append(nil))
}

// Lookup describes name in directory dir.
func (c *Client) Lookup(dir uint64, name string) (Attr, error) {
	return c.attrCall(OpLookup, dir, []byte(name))
}

// Readdir lists directory dir after the name after ("" for the first page)
// within maxBytes; eof is set when the page ends the listing.
func (c *Client) Readdir(dir uint64, after string, maxBytes uint32) (entries []Dirent, eof bool, err error) {
	b := le.AppendUint32(nil, maxBytes)
	b = le.AppendUint16(b, uint16(len(after)))
	b = le.AppendUint16(b, 0)
	res, err := c.Call(OpReaddir, dir, b, []byte(after))
	if err != nil {
		return nil, false, err
	}
	if len(res) < ReaddirRepSize {
		return nil, false, errShort
	}
	count, eof := le.Uint32(res), le.Uint32(res[4:]) != 0
	res = res[ReaddirRepSize:]
	for range count {
		var d Dirent
		n, err := d.decode(res)
		if err != nil {
			return nil, false, err
		}
		entries = append(entries, d)
		res = res[n:]
	}
	return entries, eof, nil
}

// ReaddirAll lists all of directory dir, a page of maxBytes at a time.
func (c *Client) ReaddirAll(dir uint64, maxBytes uint32) ([]Dirent, error) {
	var all []Dirent
	after := ""
	for {
		page, eof, err := c.Readdir(dir, after, maxBytes)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if eof || len(page) == 0 {
			return all, nil
		}
		after = page[len(page)-1].Name
	}
}

// Readlink returns symlink id's target.
func (c *Client) Readlink(id uint64) (string, error) {
	res, err := c.Call(OpReadlink, id)
	return string(res), err
}

// Read returns up to n bytes of file id at off, short only at end of file.
func (c *Client) Read(id, off uint64, n uint32) ([]byte, error) {
	b := le.AppendUint64(nil, off)
	b = le.AppendUint32(b, n)
	b = le.AppendUint32(b, 0)
	res, err := c.Call(OpRead, id, b)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), res...), nil
}

// Write stores data in file id at off.
func (c *Client) Write(id, off uint64, data []byte) (Attr, error) {
	b := le.AppendUint64(nil, off)
	b = le.AppendUint32(b, uint32(len(data)))
	b = le.AppendUint32(b, 0)
	return c.attrCall(OpWrite, id, b, data)
}

// Fsync makes file id's data and name durable.
func (c *Client) Fsync(id uint64) error {
	_, err := c.Call(OpFsync, id)
	return err
}

// Create creates or opens regular file name in directory dir.
func (c *Client) Create(dir uint64, name string, mode, flags uint32) (Attr, error) {
	return c.attrCall(OpCreate, dir, c.CreateBody(name, mode, flags, 0, 0, nil)...)
}

// CreateWithID creates regular file name in directory dir with id, from
// AllocIDs, mtime and contents data, exclusively.
func (c *Client) CreateWithID(dir uint64, name string, mode uint32, id uint64, mtime time.Time, data []byte) (Attr, error) {
	return c.attrCall(OpCreate, dir, c.CreateBody(name, mode, CreateExcl, id, uint64(mtime.UnixNano()), data)...)
}

// CreateBody encodes a CREATE body in parts, for Call or Request.
func (c *Client) CreateBody(name string, mode, flags uint32, id, mtimeNs uint64, data []byte) [][]byte {
	cr := CreateReq{Mode: mode, Flags: flags, NewID: id, MtimeNs: mtimeNs, DataLen: uint32(len(data)), NameLen: uint16(len(name))}
	return [][]byte{cr.append(nil), []byte(name), data}
}

// MkdirWithID creates directory name in directory dir with id, from
// AllocIDs, and mtime.
func (c *Client) MkdirWithID(dir uint64, name string, mode uint32, id uint64, mtime time.Time) (Attr, error) {
	mr := MkdirReq{Mode: mode, NewID: id, MtimeNs: uint64(mtime.UnixNano())}
	return c.attrCall(OpMkdir, dir, mr.append(nil), []byte(name))
}

// AllocIDs reserves up to count ids for files and directories the client
// creates with them, returning the first and how many there are.
func (c *Client) AllocIDs(count uint32) (first uint64, got uint32, err error) {
	res, err := c.Call(OpAllocIDs, RootID, le.AppendUint32(le.AppendUint32(nil, count), 0))
	if err != nil {
		return 0, 0, err
	}
	var a AllocRep
	return a.First, a.Count, a.decode(res)
}

// Mkdir creates directory name in directory dir.
func (c *Client) Mkdir(dir uint64, name string, mode uint32) (Attr, error) {
	mr := MkdirReq{Mode: mode}
	return c.attrCall(OpMkdir, dir, mr.append(nil), []byte(name))
}

// Mknod creates a FIFO or socket name in directory dir; mode carries its type.
func (c *Client) Mknod(dir uint64, name string, mode, rdev uint32) (Attr, error) {
	return c.attrCall(OpMknod, dir, le.AppendUint32(le.AppendUint32(nil, mode), rdev), []byte(name))
}

// Symlink creates symlink name to target in directory dir.
func (c *Client) Symlink(dir uint64, name, target string) (Attr, error) {
	b := le.AppendUint16(nil, uint16(len(name)))
	b = le.AppendUint16(b, 0)
	b = le.AppendUint32(b, 0)
	return c.attrCall(OpSymlink, dir, b, []byte(name), []byte(target))
}

// Link gives file id the name name in directory dir.
func (c *Client) Link(id, dir uint64, name string) (Attr, error) {
	return c.attrCall(OpLink, id, le.AppendUint64(nil, dir), []byte(name))
}

// Unlink removes non-directory name from directory dir.
func (c *Client) Unlink(dir uint64, name string) error {
	_, err := c.Call(OpUnlink, dir, []byte(name))
	return err
}

// Rmdir removes empty directory name from directory dir.
func (c *Client) Rmdir(dir uint64, name string) error {
	_, err := c.Call(OpRmdir, dir, []byte(name))
	return err
}

// Rename moves oldName in oldDir to newName in newDir.
func (c *Client) Rename(oldDir uint64, oldName string, newDir uint64, newName string, flags uint32) error {
	r := RenameReq{NewDir: newDir, Flags: flags, OldLen: uint16(len(oldName)), NewLen: uint16(len(newName))}
	_, err := c.Call(OpRename, oldDir, r.append(nil), []byte(oldName), []byte(newName))
	return err
}

// Request encodes a request for [Client.Compound]: op on file id with body
// parts after the header, as the client's caller.
func (c *Client) Request(op Op, id uint64, body ...[]byte) []byte {
	req := Req{Op: op, UID: c.UID, GID: c.GID, ID: id}
	b := req.append(nil)
	for _, part := range body {
		b = append(b, part...)
	}
	return b
}

// SubReply is the reply to one request of a COMPOUND.
type SubReply struct {
	// Err is the request's error; nil when it succeeded.
	Err error
	// Epoch is the reply's epoch.
	Epoch uint64
	// Result is the request's result, valid until the client's next call.
	Result []byte
}

// Compound sends reqs (see [Client.Request]) as one COMPOUND and returns
// their replies in order. The error is the compound's own: a malformed
// compound, which ran none of reqs, or a transport failure.
func (c *Client) Compound(reqs ...[]byte) ([]SubReply, error) {
	parts := make([][]byte, 0, 2*len(reqs))
	for _, r := range reqs {
		parts = append(parts, le.AppendUint32(nil, uint32(len(r))), r)
	}
	res, err := c.Call(OpCompound, 0, parts...)
	if err != nil {
		return nil, err
	}
	out := make([]SubReply, 0, len(reqs))
	for len(res) > 0 {
		if len(res) < subHeader {
			return nil, errShort
		}
		n := int(le.Uint32(res))
		res = res[subHeader:]
		if n > len(res) {
			return nil, errShort
		}
		var rep Rep
		if err := rep.decode(res[:n]); err != nil {
			return nil, err
		}
		sub := SubReply{Epoch: rep.Epoch, Result: res[RepSize:n]}
		if rep.Error != 0 {
			sub.Err = Errno(rep.Error)
		}
		out = append(out, sub)
		res = res[n:]
	}
	return out, nil
}
