package kfs

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/internal/kept"
	"github.com/stencil-hq/ovnfs/nfs"
)

// Share is one checkout as the kernel client sees it.
type Share struct {
	// Export holds the checkout. Requests go through its Quiet view, which
	// keeps file ids in step; the server recalls other protocols' cached
	// copies itself, by the ids it resolved (Export.Recall).
	Export *export.Export
	// Checkout names the served checkout; RootID is its root.
	Checkout string
	// Writable admits mutations; a read-only share answers them with EROFS.
	Writable bool
}

// Options configures a server.
type Options struct {
	// Quiesce flushes and excludes external write caches for each request,
	// as for remote-fs; its release runs after the request. Nil skips it.
	Quiesce func(context.Context) (func(), error)
	// Logf receives rejected peers and broken connections; nil discards them.
	Logf func(format string, args ...any)
}

// keptMax bounds the files a share keeps open between requests. The guest
// sends a file's reads and writes in bursts, a few files at a time per CPU.
const keptMax = 256

// fileKey names a kept file: readers and writers are kept apart, since a
// reader opened on a lower layer would not see a later copy-up, and gen
// retires every file kept before a daemon-side change.
type fileKey struct {
	id    uint64
	write bool
	gen   uint64
}

// server is the state every connection of one share uses.
type server struct {
	share Share
	opts  Options
	nodes *export.Nodes
	// syncer commits files as an NFS COMMIT does (nfs.Backend.SyncFile).
	syncer nfs.Backend
	// base is this run's random epoch origin, so a restart or another share
	// never repeats an epoch.
	base uint64
	// gen counts daemon-side changes of the checkout (Export.OnInvalidate);
	// the epoch is base+gen.
	gen atomic.Uint64
	// changes counts this share's own mutations, which a kept listing must
	// not outlive.
	changes atomic.Uint64
	// files keeps files open across requests of every connection, so a
	// write on one CPU can drop a reader another CPU kept.
	files kept.Cache[fileKey]
}

// Serve answers kernel-client connections from l for share until l is
// closed, then closes the open connections and returns nil. Each connection
// is one VMM port: strictly alternating, length-prefixed request and reply
// frames. Peers running as another user are dropped.
func Serve(l net.Listener, share Share, opts Options) error {
	if share.Export == nil {
		return errors.New("kfs: share export is required")
	}
	if !validCheckout(share.Checkout) {
		return fmt.Errorf("kfs: invalid checkout name %q", share.Checkout)
	}
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return fmt.Errorf("kfs: epoch seed: %w", err)
	}
	s := &server{
		share:  share,
		opts:   opts,
		nodes:  share.Export.Quiet(),
		syncer: share.Export.NFS(),
		base:   le.Uint64(seed[:]),
		files:  kept.Cache[fileKey]{Max: keptMax},
	}
	defer share.Export.OnInvalidate(share.Checkout, func() { s.gen.Add(1) })()
	defer s.files.Close()
	var (
		mu    sync.Mutex
		conns = make(map[net.Conn]struct{})
		wg    sync.WaitGroup
		delay time.Duration
	)
	defer func() {
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				time.Sleep(delay)
				continue
			}
			return fmt.Errorf("kfs: accept: %w", err)
		}
		delay = 0
		mu.Lock()
		conns[c] = struct{}{}
		mu.Unlock()
		wg.Go(func() {
			defer func() {
				mu.Lock()
				delete(conns, c)
				mu.Unlock()
				c.Close()
			}()
			sizeBuffers(c)
			if err := admin.CheckPeer(c); err != nil {
				s.logf("kfs %s: reject peer: %v", s.share.Checkout, err)
				return
			}
			if err := s.serveConn(c); err != nil {
				s.logf("kfs %s: connection: %v", s.share.Checkout, err)
			}
		})
	}
}

// logf forwards to Options.Logf when set.
func (s *server) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// conn is one connection's buffers and listing.
type conn struct {
	*server
	in  frames // request frames
	out []byte // the reply frame: byte count, ovnfs_rep, result
	gen uint64 // s.gen when the request arrived
	// list is the connection's last directory listing (see readdir).
	list *listing
}

// replyStart is where a reply's result starts in conn.out.
const replyStart = frameHeader + RepSize

// serveConn answers frames in order until the peer closes or sends a frame
// that breaks framing. A WRITE's payload is read from the socket straight
// into the connection's frame buffer and written to the file from there; a
// reply, a READ's data included, is built in one buffer and leaves in one
// write.
func (s *server) serveConn(nc net.Conn) error {
	c := &conn{server: s, out: make([]byte, replyStart, frameStart)}
	for {
		frame, err := c.in.read(nc)
		if err != nil {
			if err == io.EOF || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read frame: %w", err)
		}
		c.handle(frame)
		if _, err := nc.Write(c.out); err != nil {
			return fmt.Errorf("write reply: %w", err)
		}
	}
}

// handle runs one request and leaves its reply frame in c.out. The epoch is
// read before the request touches the tree: a daemon-side change that lands
// meanwhile then shows in the next reply's epoch, never only in this one's.
func (c *conn) handle(frame []byte) {
	c.gen = c.server.gen.Load()
	c.out = c.out[:replyStart]
	err := c.dispatch(frame)
	if err != nil {
		c.out = c.out[:replyStart]
	}
	le.PutUint32(c.out, uint32(len(c.out)-frameHeader))
	rep := Rep{Error: uint32(errnoOf(err)), Epoch: c.base + c.gen}
	rep.put(c.out[frameHeader:])
}

// dispatch decodes the request header, refuses mutations of a read-only
// share and runs the op.
func (c *conn) dispatch(frame []byte) error {
	var req Req
	if err := req.decode(frame); err != nil {
		return EINVAL
	}
	if mutates(req.Op) {
		if !c.share.Writable {
			return EROFS
		}
		// Bumped after the change, so a listing read before it is dropped.
		defer c.changes.Add(1)
	}
	if c.opts.Quiesce != nil {
		release, err := c.opts.Quiesce(context.Background())
		if err != nil {
			return err
		}
		defer release()
	}
	return c.run(&req, frame[ReqSize:])
}

// mutates reports whether op changes the tree.
func mutates(op Op) bool {
	switch op {
	case OpSetattr, OpWrite, OpCreate, OpMkdir, OpMknod, OpSymlink, OpLink, OpUnlink, OpRmdir, OpRename:
		return true
	}
	return false
}

// validCheckout reports whether name can be a checkout: one path segment.
func validCheckout(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for i := range len(name) {
		if name[i] == '/' || name[i] == 0 {
			return false
		}
	}
	return true
}
