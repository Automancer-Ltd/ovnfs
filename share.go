package main

import (
	"context"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
	"github.com/stencil-hq/ovnfs/export"
	"github.com/stencil-hq/ovnfs/kfs"
	"github.com/stencil-hq/ovnfs/remotefs"
)

// shares serves checkouts over sockets in <state>/shares: remote-fs as
// <checkout>.sock, the guest kernel client's protocol as kernel/<checkout>.sock,
// at most one of each per checkout.
type shares struct {
	dir string
	reg *checkout.Registry
	e   *export.Export
	mu  sync.Mutex
	on  map[shareKey]share
}

// shareKey names one share: a checkout and the protocol it is served with.
type shareKey struct {
	checkout string
	kernel   bool
}

// share is one live share listener.
type share struct {
	l        net.Listener
	writable bool
}

// newShares prepares the socket directories, removing sockets a previous run left.
func newShares(state string, reg *checkout.Registry, e *export.Export) (*shares, error) {
	dir := filepath.Join(state, "shares")
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "kernel"), 0o700); err != nil {
		return nil, err
	}
	return &shares{dir: dir, reg: reg, e: e, on: map[shareKey]share{}}, nil
}

// Share starts (or replaces) a checkout's remote-fs or kernel-client socket.
func (s *shares) Share(_ context.Context, args api.ShareArgs) (api.ShareReply, error) {
	if _, ok := s.reg.Gen(args.Checkout); !ok {
		return api.ShareReply{}, &checkout.Error{Code: "not_found", Message: "no checkout " + args.Checkout}
	}
	key := shareKey{checkout: args.Checkout, kernel: args.Kernel}
	sock := filepath.Join(s.dir, args.Checkout+".sock")
	listen := remotefs.Listen
	if args.Kernel {
		sock = filepath.Join(s.dir, "kernel", args.Checkout+".sock")
		listen = admin.Listen
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.on[key]; ok {
		if old.writable == args.Writable {
			return api.ShareReply{Socket: sock, Writable: args.Writable}, nil
		}
		old.l.Close()
		delete(s.on, key)
	}
	l, err := listen(sock)
	if err != nil {
		return api.ShareReply{}, &checkout.Error{Code: "io", Message: err.Error()}
	}
	s.on[key] = share{l: l, writable: args.Writable}
	go func() {
		var err error
		if args.Kernel {
			err = kfs.Serve(l, kfs.Share{Export: s.e, Checkout: args.Checkout, Writable: args.Writable}, kfs.Options{Logf: log.Printf, Quiesce: s.reg.Quiesce})
		} else {
			err = remotefs.Serve(l, remotefs.Share{FS: s.e.Nodes(), Checkout: args.Checkout, Writable: args.Writable}, remotefs.Options{Logf: log.Printf, Quiesce: s.reg.Quiesce})
		}
		if err != nil {
			log.Printf("share %s: %v", args.Checkout, err)
		}
	}()
	return api.ShareReply{Socket: sock, Writable: args.Writable}, nil
}

// Close stops a checkout's shares, if any; removed checkouts call it.
func (s *shares) Close(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, kernel := range []bool{false, true} {
		key := shareKey{checkout: name, kernel: kernel}
		if sh, ok := s.on[key]; ok {
			sh.l.Close()
			delete(s.on, key)
		}
	}
}

// CloseAll stops every share at shutdown.
func (s *shares) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, sh := range s.on {
		sh.l.Close()
		delete(s.on, key)
	}
}
