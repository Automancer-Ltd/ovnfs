package nfs

import "sync/atomic"

// dirChanges tells READDIRPLUS which attributes in a snapshot may be stale.
// A namespace change or an external recall invalidates all attributes. A
// regular-file WRITE or SETATTR only invalidates that file, including every
// hard link to it. Hash collisions cost an extra stat, never a stale reply.
type dirChanges struct {
	clock atomic.Uint64
	all   atomic.Uint64
	files [1024]atomic.Uint64
}

// file returns the change stamp shared by ids in the same bucket. Fold the
// high bits so consecutive checkout slots do not all alias the same bucket.
func (c *dirChanges) file(id uint64) *atomic.Uint64 {
	return &c.files[(id^(id>>32))%uint64(len(c.files))]
}

// mark publishes a mutation in the clock used to date directory snapshots.
// Two writers can reach a bucket in reverse clock order; never move it back.
func (c *dirChanges) mark(bucket *atomic.Uint64) {
	at := c.clock.Add(1)
	for last := bucket.Load(); last < at && !bucket.CompareAndSwap(last, at); last = bucket.Load() {
	}
}

// invalidate covers changes whose effects are not limited to one regular file.
func (c *dirChanges) invalidate() { c.mark(&c.all) }
