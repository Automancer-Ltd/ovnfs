package nfs

import "sync"

// fileOrder runs the NFSv3 calls on each file in the order they arrived,
// where that order shows in replies.
//
// A WRITE or SETATTR reply carries the file's attributes from just before and
// just after the change (wcc_data). The macOS client counts a change as its
// own only when the pre-op mtime equals the one it recorded last, and keeps
// attributes only from the reply with the newest xid. Otherwise it treats
// its own write as another host's and drops every cached page of the file
// at its next attribute check. So changes to one file run one at a time, in
// arrival order, each reading the attributes around itself, and calls that
// report the file's attributes wait for the changes that arrived before
// them. Calls on other files, and reports on the same file, still run
// concurrently.
type fileOrder struct {
	mu   sync.Mutex
	last map[uint64]*change // newest unanswered change entered per file id
}

// change is one entered SETATTR or WRITE; done closes once it is answered.
type change struct {
	id   uint64
	done chan struct{}
}

// turn is one call's place in its file's order. The zero turn waits for
// nothing and releases nothing.
type turn struct {
	order *fileOrder
	after *change // the newest change entered before this call
	own   *change // this call's change; nil for a call that only reports
}

// enter places a call on file id behind every change entered before it. A
// call that changes the file becomes the change later calls wait for.
func (o *fileOrder) enter(id uint64, changes bool) turn {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := turn{order: o, after: o.last[id]}
	if changes {
		t.own = &change{id: id, done: make(chan struct{})}
		if o.last == nil {
			o.last = make(map[uint64]*change)
		}
		o.last[id] = t.own
	}
	return t
}

// wait blocks until the change entered before t has been answered; changes
// wait for their predecessors, so that covers every earlier one.
func (t turn) wait() {
	if t.after != nil {
		<-t.after.done
	}
}

// leave releases the calls waiting for t's change.
func (t turn) leave() {
	if t.own == nil {
		return
	}
	close(t.own.done)
	o := t.order
	o.mu.Lock()
	if o.last[t.own.id] == t.own {
		delete(o.last, t.own.id)
	}
	o.mu.Unlock()
}
