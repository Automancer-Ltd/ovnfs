package kfs

import (
	"context"
	"math"
)

// EMSGSIZE is Linux's "message too long", the error of a COMPOUND
// sub-request whose reply could not fit the reply frame.
const EMSGSIZE Errno = 90

// subHeader is the byte count before each sub-request and sub-reply of a COMPOUND.
const subHeader = 4

// compound runs COMPOUND request frame: its sub-requests in order, as if
// each had arrived alone, appending the outer reply header and then each
// sub-reply with its byte count. A malformed compound (a sub-request cut
// short or nested COMPOUND) executes nothing and fails as a whole with
// EINVAL. A sub-request whose largest possible reply would not fit what is
// left of the reply frame is not executed and fails alone with EMSGSIZE, so
// the reply frame never outgrows MaxFrame.
func (c *conn) compound(frame []byte) {
	c.gen = c.server.gen.Load()
	outer := len(c.out)
	c.out = append(c.out, make([]byte, RepSize)...)
	subs, ok := splitCompound(frame[ReqSize:])
	var err error
	if !ok {
		err = EINVAL
	} else {
		err = c.runCompound(subs)
	}
	if err != nil {
		c.out = c.out[:outer+RepSize]
	}
	rep := Rep{Error: uint32(errnoOf(err)), Epoch: c.base + c.gen}
	rep.put(c.out[outer:])
}

// runCompound runs the sub-requests of a well-formed COMPOUND, holding the
// write-cache barrier once for all of them; only a failure to take the
// barrier fails the compound, before anything ran.
func (c *conn) runCompound(subs [][]byte) error {
	if c.opts.Quiesce != nil {
		release, err := c.opts.Quiesce(context.Background())
		if err != nil {
			return err
		}
		defer release()
		c.quiesced = true
		defer func() { c.quiesced = false }()
	}
	for _, sub := range subs {
		at := len(c.out)
		c.out = append(c.out, 0, 0, 0, 0)
		if frameHeader+len(c.out)+maxReply(sub) > frameHeader+MaxFrame {
			c.gen = c.server.gen.Load()
			rep := Rep{Error: uint32(EMSGSIZE), Epoch: c.base + c.gen}
			c.out = append(c.out, make([]byte, RepSize)...)
			rep.put(c.out[at+subHeader:])
		} else {
			c.reply(sub)
		}
		le.PutUint32(c.out[at:], uint32(len(c.out)-at-subHeader))
	}
	return nil
}

// splitCompound returns the sub-requests of a COMPOUND body; false when one
// is cut short, too short to hold a request header, or itself a COMPOUND.
func splitCompound(body []byte) ([][]byte, bool) {
	var subs [][]byte
	for len(body) > 0 {
		if len(body) < subHeader {
			return nil, false
		}
		n := le.Uint32(body)
		body = body[subHeader:]
		if uint64(n) > uint64(len(body)) {
			return nil, false
		}
		sub := body[:n]
		body = body[n:]
		if op, ok := opOf(sub); !ok || op == OpCompound {
			return nil, false
		}
		subs = append(subs, sub)
	}
	return subs, true
}

// maxReply bounds the reply of request req, header included, by what its op
// can return: a malformed request gets a bare header.
func maxReply(req []byte) int {
	op, _ := opOf(req)
	body := req[min(ReqSize, len(req)):]
	result := AttrSize
	switch op {
	case OpHello:
		result = HelloRepSize
	case OpStatfs:
		result = StatfsRepSize
	case OpAllocIDs:
		result = AllocRepSize
	case OpPoll, OpFsync, OpUnlink, OpRmdir, OpRename:
		result = 0
	case OpReadlink:
		result = symlinkMax
	case OpRead:
		if len(body) >= ReadReqSize {
			result = int(min(le.Uint32(body[8:]), math.MaxInt32))
		}
	case OpReaddir:
		// A page holds at least one entry, however small the budget.
		result = ReaddirRepSize + direntSize(NameMax)
		if len(body) >= ReaddirReqSize {
			result = max(result, int(min(le.Uint32(body), math.MaxInt32)))
		}
	}
	return subHeader + RepSize + result
}
