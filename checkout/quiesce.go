package checkout

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// quiesceLimit bounds how long Quiesce waits for write caches to drain.
const quiesceLimit = 5 * time.Second

// Quiesce drains client write caches before a direct filesystem operation. Its
// release must be held through that operation, even after ctx expires. The
// export's counted grant gate permits concurrent and nested acquisitions; this
// wrapper must never serialize them or acquire registry/freeze locks, because
// the client's WRITE and FLUSH requests still need Enter while draining.
//
// Acquisition is bounded to five seconds or the caller's earlier deadline.
// The bound arms a timer only once the gate waits on its context: a file
// server calls this for every request, and when nothing is cached the gate
// returns without looking, which then costs no timer (arming one wakes the
// runtime's network poller, several microseconds on a request). A nil
// Config.Quiesce is safe only for exports that never grant write caching.
func (r *Registry) Quiesce(ctx context.Context) (func(), error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.config.Quiesce == nil {
		return func() {}, nil
	}
	wait := newBounded(ctx, quiesceLimit)
	defer wait.cancel()
	release, err := r.config.Quiesce(wait)
	if err == nil {
		err = wait.expired()
	}
	if err != nil {
		if release != nil {
			release()
		}
		return nil, coded(err)
	}
	if release == nil {
		return nil, failure("io", "write-cache quiesce returned no release")
	}
	return release, nil
}

// bounded is context.WithTimeout(parent, limit) whose timer, and the
// derived context holding it, exist only once Done or Err is called.
// Deadline answers without them, so a callee may still bound its own waits.
type bounded struct {
	parent   context.Context
	deadline time.Time
	mu       sync.Mutex // serializes arming
	armed    atomic.Pointer[armedContext]
	canceled atomic.Bool
}

// armedContext is the derived context once armed and its cancel.
type armedContext struct {
	ctx  context.Context
	stop context.CancelFunc
}

// newBounded returns parent bounded to limit from now or parent's earlier deadline.
func newBounded(parent context.Context, limit time.Duration) *bounded {
	deadline := time.Now().Add(limit)
	if d, ok := parent.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return &bounded{parent: parent, deadline: deadline}
}

// Deadline reports the bound without arming it.
func (b *bounded) Deadline() (time.Time, bool) { return b.deadline, true }

// Done arms the bound and returns its channel.
func (b *bounded) Done() <-chan struct{} { return b.arm().Done() }

// Err arms the bound and reports why it is done.
func (b *bounded) Err() error { return b.arm().Err() }

// Value forwards to the parent.
func (b *bounded) Value(key any) any { return b.parent.Value(key) }

// arm returns the derived context, creating it on first use. One armed
// after cancel is canceled at once, as WithTimeout's would be by then.
func (b *bounded) arm() context.Context {
	if a := b.armed.Load(); a != nil {
		return a.ctx
	}
	b.mu.Lock()
	a := b.armed.Load()
	if a == nil {
		ctx, stop := context.WithDeadline(b.parent, b.deadline)
		a = &armedContext{ctx: ctx, stop: stop}
		b.armed.Store(a)
	}
	b.mu.Unlock()
	// Either cancel sees the stored context or this sees canceled.
	if b.canceled.Load() {
		a.stop()
	}
	return a.ctx
}

// cancel releases the timer, if armed, once the gate returned.
func (b *bounded) cancel() {
	b.canceled.Store(true)
	if a := b.armed.Load(); a != nil {
		a.stop()
	}
}

// expired reports what the derived context's Err would after the gate
// returned: the parent's error, or DeadlineExceeded past the bound. It arms
// nothing that was not armed.
func (b *bounded) expired() error {
	if a := b.armed.Load(); a != nil {
		return a.ctx.Err()
	}
	if err := b.parent.Err(); err != nil {
		return err
	}
	if !time.Now().Before(b.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
