// Package kept retains open files between the requests of stateless file
// protocols (NFSv3, the kernel client's), which name files but never open or
// close them: opening and closing a host descriptor per request costs more
// than the request.
package kept

import (
	"container/list"
	"io"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
)

// Defaults of a zero [Cache]'s limits.
const (
	// DefaultIdle is how long a kept file outlives its last use. Clients
	// send a file's cached writes when it is closed, so the writes of one
	// open come in a burst.
	DefaultIdle = 10 * time.Second
	// DefaultMax bounds the files kept at once; past it the least recently
	// used idle one is closed.
	DefaultMax = 4096
)

// Cache keeps open files by key, closing each once it has been idle for
// Idle, and the least recently used idle one when Max files are kept. The
// zero value is ready to use with the default limits; set the limits before
// first use.
type Cache[K comparable] struct {
	// Idle is how long a kept file outlives its last use; zero: DefaultIdle.
	Idle time.Duration
	// Max bounds the files kept at once; zero: DefaultMax.
	Max int

	mu    sync.Mutex
	files map[K]*list.Element // of *entry[K]
	lru   list.List           // most recently used first
}

// entry is one kept file. It closes once idle with no call under way, or
// once dropped and its last call returned.
type entry[K comparable] struct {
	mu    sync.Mutex // serializes calls, e.g. seek/write for files without WriteAt
	key   K
	f     billy.File
	users int
	gone  bool // dropped while in use: the last user closes it
	timer *time.Timer
}

// Has reports whether key has a kept file.
func (c *Cache[K]) Has(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.files[key]
	return ok
}

// Keep takes ownership of f, closing it if key is already cached or every
// slot is busy. Any descriptor closed here reports its error to the caller.
func (c *Cache[K]) Keep(key K, f billy.File) error {
	c.mu.Lock()
	if _, ok := c.files[key]; ok {
		c.mu.Unlock()
		return f.Close()
	}
	if c.files == nil {
		c.files = make(map[K]*list.Element)
	}
	var evicted billy.File
	if len(c.files) >= c.max() {
		evicted = c.evictLocked()
		if evicted == nil {
			c.mu.Unlock()
			return f.Close()
		}
	}
	e := &entry[K]{key: key, f: f}
	c.files[key] = c.lru.PushFront(e)
	e.timer = time.AfterFunc(c.idle(), func() { c.expire(e) })
	c.mu.Unlock()
	if evicted != nil {
		return evicted.Close()
	}
	return nil
}

// With serializes fn on key's kept file; ok is false when there is none.
func (c *Cache[K]) With(key K, fn func(billy.File) error) (ok bool, err error) {
	c.mu.Lock()
	el := c.files[key]
	if el == nil {
		c.mu.Unlock()
		return false, nil
	}
	e := el.Value.(*entry[K])
	e.users++
	c.lru.MoveToFront(el)
	c.mu.Unlock()

	e.mu.Lock()
	err = fn(e.f)
	e.mu.Unlock()

	c.mu.Lock()
	e.users--
	closing := e.gone && e.users == 0
	if !e.gone {
		e.timer.Reset(c.idle())
	}
	c.mu.Unlock()
	if closing {
		e.f.Close()
	}
	return true, err
}

// Drop forgets key's kept file and closes it, at once when it is idle, else
// after the call using it returns. Callers drop a file whose name or id went
// away, or whose descriptor no longer shows the file's current content.
func (c *Cache[K]) Drop(key K) error {
	c.mu.Lock()
	el := c.files[key]
	if el == nil {
		c.mu.Unlock()
		return nil
	}
	e := c.removeLocked(el)
	busy := e.users > 0
	c.mu.Unlock()
	if busy {
		return nil
	}
	return e.f.Close()
}

// Close drops every kept file, as a server's process exiting would: Windows
// cannot remove a folder while a file in it is open.
func (c *Cache[K]) Close() {
	c.mu.Lock()
	var idle []billy.File
	for el := c.lru.Front(); el != nil; el = c.lru.Front() {
		if e := c.removeLocked(el); e.users == 0 {
			idle = append(idle, e.f)
		}
	}
	c.mu.Unlock()
	for _, f := range idle {
		f.Close()
	}
}

// removeLocked unlinks el's entry and marks it gone; the caller closes it
// when it has no users.
func (c *Cache[K]) removeLocked(el *list.Element) *entry[K] {
	e := el.Value.(*entry[K])
	e.timer.Stop()
	e.gone = true
	c.lru.Remove(el)
	delete(c.files, e.key)
	return e
}

// expire closes e once it is idle.
func (c *Cache[K]) expire(e *entry[K]) {
	c.mu.Lock()
	el := c.files[e.key]
	if el == nil || el.Value != e {
		c.mu.Unlock()
		return
	}
	if e.users > 0 {
		e.timer.Reset(c.idle())
		c.mu.Unlock()
		return
	}
	c.removeLocked(el)
	c.mu.Unlock()
	e.f.Close()
}

// evictLocked drops the least recently used idle file and returns it for
// the caller to close outside the lock; nil when every file is in use.
func (c *Cache[K]) evictLocked() billy.File {
	for el := c.lru.Back(); el != nil; el = el.Prev() {
		if e := el.Value.(*entry[K]); e.users == 0 {
			c.removeLocked(el)
			return e.f
		}
	}
	return nil
}

// idle is the configured idle lifetime.
func (c *Cache[K]) idle() time.Duration {
	if c.Idle > 0 {
		return c.Idle
	}
	return DefaultIdle
}

// max is the configured bound.
func (c *Cache[K]) max() int {
	if c.Max > 0 {
		return c.Max
	}
	return DefaultMax
}

// WriteAt writes data at offset of f, without moving an offset others
// share where the file can.
func WriteAt(f billy.File, data []byte, offset int64) (int, error) {
	if w, ok := f.(io.WriterAt); ok {
		return w.WriteAt(data, offset)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return f.Write(data)
}
