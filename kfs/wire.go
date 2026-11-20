// Package kfs serves one checkout to the ovnfs guest kernel client, the
// filesystem a vibevmm microVM reaches by trapping into the VMM, which relays
// each request over a Unix socket. kmod/ovnfs_wire.h defines the protocol;
// this file mirrors it.
package kfs

import (
	"encoding/binary"
	"errors"
)

// Framing and limits (ovnfs_wire.h).
const (
	// ProtoVersion is the protocol version this package speaks.
	ProtoVersion = 1
	// RootID names the served checkout's root.
	RootID uint64 = 1
	// NameMax is the longest name, in bytes.
	NameMax = 255
	// MaxIO is the largest READ or WRITE payload of one request.
	MaxIO = 1 << 20
	// MaxFrame is the largest request or reply, headers included.
	MaxFrame = MaxIO + 4096
	// frameHeader is the byte count before every frame.
	frameHeader = 4
)

// Op is an ovnfs_op.
type Op uint16

// Ops (enum ovnfs_op).
const (
	OpHello    Op = 1
	OpPoll     Op = 2
	OpStatfs   Op = 3
	OpGetattr  Op = 4
	OpSetattr  Op = 5
	OpLookup   Op = 6
	OpReaddir  Op = 7
	OpReadlink Op = 8
	OpRead     Op = 9
	OpWrite    Op = 10
	OpFsync    Op = 11
	OpCreate   Op = 12
	OpMkdir    Op = 13
	OpMknod    Op = 14
	OpSymlink  Op = 15
	OpLink     Op = 16
	OpUnlink   Op = 17
	OpRmdir    Op = 18
	OpRename   Op = 19
)

// Flags.
const (
	// HelloRdonly: the tree refuses mutations with EROFS.
	HelloRdonly uint32 = 1 << 0

	SetMode     uint32 = 1 << 0
	SetUID      uint32 = 1 << 1
	SetGID      uint32 = 1 << 2
	SetSize     uint32 = 1 << 3
	SetMtime    uint32 = 1 << 4 // to SetattrReq.MtimeNs
	SetMtimeNow uint32 = 1 << 5 // to the daemon's clock

	CreateExcl  uint32 = 1 << 0
	CreateTrunc uint32 = 1 << 1 // empty an existing file

	RenameNoreplace uint32 = 1 << 0
)

// Linux S_IFMT file types, as ovnfs_attr.mode carries them.
const (
	sIFMT   = 0o170000
	sIFSOCK = 0o140000
	sIFLNK  = 0o120000
	sIFREG  = 0o100000
	sIFBLK  = 0o060000
	sIFDIR  = 0o040000
	sIFCHR  = 0o020000
	sIFIFO  = 0o010000
)

// Encoded sizes of the fixed structs.
const (
	ReqSize        = 24
	RepSize        = 16
	AttrSize       = 64
	HelloReqSize   = 8
	HelloRepSize   = 16 + AttrSize
	StatfsRepSize  = 48
	SetattrReqSize = 32
	ReaddirReqSize = 8
	ReaddirRepSize = 8
	// DirentHeader is where a dirent's name starts: its attr and name_len.
	DirentHeader   = AttrSize + 2
	ReadReqSize    = 16
	WriteReqSize   = 16
	CreateReqSize  = 8
	MkdirReqSize   = 8
	MknodReqSize   = 8
	SymlinkReqSize = 8
	LinkReqSize    = 8
	RenameReqSize  = 16
)

// errShort reports a request or reply shorter than its op's fixed part.
var errShort = errors.New("kfs: message too short")

// le is the byte order of every integer on the wire.
var le = binary.LittleEndian

// Req is struct ovnfs_req.
type Req struct {
	Op  Op
	UID uint32
	GID uint32
	ID  uint64
}

// append encodes r.
func (r *Req) append(b []byte) []byte {
	b = le.AppendUint16(b, uint16(r.Op))
	b = le.AppendUint16(b, 0)
	b = le.AppendUint32(b, r.UID)
	b = le.AppendUint32(b, r.GID)
	b = le.AppendUint32(b, 0)
	return le.AppendUint64(b, r.ID)
}

// decode reads r from the start of b.
func (r *Req) decode(b []byte) error {
	if len(b) < ReqSize {
		return errShort
	}
	r.Op = Op(le.Uint16(b))
	r.UID = le.Uint32(b[4:])
	r.GID = le.Uint32(b[8:])
	r.ID = le.Uint64(b[16:])
	return nil
}

// Rep is struct ovnfs_rep.
type Rep struct {
	Error uint32
	Epoch uint64
}

// put encodes r at the start of b.
func (r *Rep) put(b []byte) {
	le.PutUint32(b, r.Error)
	le.PutUint32(b[4:], 0)
	le.PutUint64(b[8:], r.Epoch)
}

// decode reads r from the start of b.
func (r *Rep) decode(b []byte) error {
	if len(b) < RepSize {
		return errShort
	}
	r.Error = le.Uint32(b)
	r.Epoch = le.Uint64(b[8:])
	return nil
}

// Attr is struct ovnfs_attr.
type Attr struct {
	ID      uint64
	Size    uint64
	Blocks  uint64
	MtimeNs uint64
	CtimeNs uint64
	Mode    uint32
	Nlink   uint32
	UID     uint32
	GID     uint32
	Rdev    uint32
}

// append encodes a.
func (a *Attr) append(b []byte) []byte {
	b = le.AppendUint64(b, a.ID)
	b = le.AppendUint64(b, a.Size)
	b = le.AppendUint64(b, a.Blocks)
	b = le.AppendUint64(b, a.MtimeNs)
	b = le.AppendUint64(b, a.CtimeNs)
	b = le.AppendUint32(b, a.Mode)
	b = le.AppendUint32(b, a.Nlink)
	b = le.AppendUint32(b, a.UID)
	b = le.AppendUint32(b, a.GID)
	b = le.AppendUint32(b, a.Rdev)
	return le.AppendUint32(b, 0)
}

// decode reads a from the start of b.
func (a *Attr) decode(b []byte) error {
	if len(b) < AttrSize {
		return errShort
	}
	a.ID = le.Uint64(b)
	a.Size = le.Uint64(b[8:])
	a.Blocks = le.Uint64(b[16:])
	a.MtimeNs = le.Uint64(b[24:])
	a.CtimeNs = le.Uint64(b[32:])
	a.Mode = le.Uint32(b[40:])
	a.Nlink = le.Uint32(b[44:])
	a.UID = le.Uint32(b[48:])
	a.GID = le.Uint32(b[52:])
	a.Rdev = le.Uint32(b[56:])
	return nil
}

// IsDir reports whether a describes a directory.
func (a *Attr) IsDir() bool { return a.Mode&sIFMT == sIFDIR }

// IsSymlink reports whether a describes a symlink.
func (a *Attr) IsSymlink() bool { return a.Mode&sIFMT == sIFLNK }

// HelloRep is struct ovnfs_hello_rep.
type HelloRep struct {
	Version uint32
	Flags   uint32
	MaxIO   uint32
	Root    Attr
}

// append encodes h.
func (h *HelloRep) append(b []byte) []byte {
	b = le.AppendUint32(b, h.Version)
	b = le.AppendUint32(b, h.Flags)
	b = le.AppendUint32(b, h.MaxIO)
	b = le.AppendUint32(b, 0)
	return h.Root.append(b)
}

// decode reads h from the start of b.
func (h *HelloRep) decode(b []byte) error {
	if len(b) < HelloRepSize {
		return errShort
	}
	h.Version = le.Uint32(b)
	h.Flags = le.Uint32(b[4:])
	h.MaxIO = le.Uint32(b[8:])
	return h.Root.decode(b[16:])
}

// StatfsRep is struct ovnfs_statfs_rep.
type StatfsRep struct {
	Blocks  uint64
	Bfree   uint64
	Bavail  uint64
	Files   uint64
	Ffree   uint64
	Bsize   uint32
	Namelen uint32
}

// append encodes s.
func (s *StatfsRep) append(b []byte) []byte {
	b = le.AppendUint64(b, s.Blocks)
	b = le.AppendUint64(b, s.Bfree)
	b = le.AppendUint64(b, s.Bavail)
	b = le.AppendUint64(b, s.Files)
	b = le.AppendUint64(b, s.Ffree)
	b = le.AppendUint32(b, s.Bsize)
	return le.AppendUint32(b, s.Namelen)
}

// decode reads s from the start of b.
func (s *StatfsRep) decode(b []byte) error {
	if len(b) < StatfsRepSize {
		return errShort
	}
	s.Blocks = le.Uint64(b)
	s.Bfree = le.Uint64(b[8:])
	s.Bavail = le.Uint64(b[16:])
	s.Files = le.Uint64(b[24:])
	s.Ffree = le.Uint64(b[32:])
	s.Bsize = le.Uint32(b[40:])
	s.Namelen = le.Uint32(b[44:])
	return nil
}

// SetattrReq is struct ovnfs_setattr_req.
type SetattrReq struct {
	Valid   uint32
	Mode    uint32
	UID     uint32
	GID     uint32
	Size    uint64
	MtimeNs uint64
}

// append encodes s.
func (s *SetattrReq) append(b []byte) []byte {
	b = le.AppendUint32(b, s.Valid)
	b = le.AppendUint32(b, s.Mode)
	b = le.AppendUint32(b, s.UID)
	b = le.AppendUint32(b, s.GID)
	b = le.AppendUint64(b, s.Size)
	return le.AppendUint64(b, s.MtimeNs)
}

// decode reads s from the start of b.
func (s *SetattrReq) decode(b []byte) error {
	if len(b) < SetattrReqSize {
		return errShort
	}
	s.Valid = le.Uint32(b)
	s.Mode = le.Uint32(b[4:])
	s.UID = le.Uint32(b[8:])
	s.GID = le.Uint32(b[12:])
	s.Size = le.Uint64(b[16:])
	s.MtimeNs = le.Uint64(b[24:])
	return nil
}

// RenameReq is struct ovnfs_rename_req.
type RenameReq struct {
	NewDir uint64
	Flags  uint32
	OldLen uint16
	NewLen uint16
}

// append encodes r.
func (r *RenameReq) append(b []byte) []byte {
	b = le.AppendUint64(b, r.NewDir)
	b = le.AppendUint32(b, r.Flags)
	b = le.AppendUint16(b, r.OldLen)
	return le.AppendUint16(b, r.NewLen)
}

// decode reads r from the start of b.
func (r *RenameReq) decode(b []byte) error {
	if len(b) < RenameReqSize {
		return errShort
	}
	r.NewDir = le.Uint64(b)
	r.Flags = le.Uint32(b[8:])
	r.OldLen = le.Uint16(b[12:])
	r.NewLen = le.Uint16(b[14:])
	return nil
}

// Dirent is one READDIR entry, struct ovnfs_dirent and its name.
type Dirent struct {
	Attr Attr
	Name string
}

// direntSize is the encoded size of an entry with an n-byte name: its
// header and name, padded to a multiple of 8.
func direntSize(n int) int { return (DirentHeader + n + 7) &^ 7 }

// append encodes d with its padding.
func (d *Dirent) append(b []byte) []byte {
	b = d.Attr.append(b)
	b = le.AppendUint16(b, uint16(len(d.Name)))
	b = append(b, d.Name...)
	for range direntSize(len(d.Name)) - DirentHeader - len(d.Name) {
		b = append(b, 0)
	}
	return b
}

// decode reads d from the start of b and returns the bytes it took.
func (d *Dirent) decode(b []byte) (int, error) {
	if len(b) < DirentHeader {
		return 0, errShort
	}
	if err := d.Attr.decode(b); err != nil {
		return 0, err
	}
	n := int(le.Uint16(b[AttrSize:]))
	size := direntSize(n)
	if len(b) < size {
		return 0, errShort
	}
	d.Name = string(b[DirentHeader : DirentHeader+n])
	return size, nil
}

// body is a request or reply body read field by field. A read past its end
// sets short and yields zeros, so a decoder checks once at the end.
type body struct {
	b     []byte
	short bool
}

// take returns the next n bytes.
func (d *body) take(n int) []byte {
	if n > len(d.b) {
		d.short = true
		d.b = nil
		return nil
	}
	v := d.b[:n:n]
	d.b = d.b[n:]
	return v
}

// u16 reads a __le16.
func (d *body) u16() uint16 {
	if v := d.take(2); v != nil {
		return le.Uint16(v)
	}
	return 0
}

// u32 reads a __le32.
func (d *body) u32() uint32 {
	if v := d.take(4); v != nil {
		return le.Uint32(v)
	}
	return 0
}

// u64 reads a __le64.
func (d *body) u64() uint64 {
	if v := d.take(8); v != nil {
		return le.Uint64(v)
	}
	return 0
}
