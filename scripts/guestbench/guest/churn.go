//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// Shape of verify.churn.
const (
	churnWorkers = 4    // concurrent workers, each in its own subtree
	churnOps     = 4000 // operations per worker
	churnEvery   = 200  // operations between full comparisons
	// churnSeed is the base seed; worker i runs churnWorkerSeed(i). Set it to
	// a seed a failure printed to replay that run's sequences.
	churnSeed uint64 = 0x6f766e6673
	// churnLate is how long the last comparison waits after syncfs, so
	// changes a filesystem applies late show up.
	churnLate = 2 * time.Second
)

// churnNames are the names every path component is drawn from: few, so
// operations collide often.
var churnNames = []string{"a", "b", "c", "d", "e"}

// Permission bits churn gives files and directories; the owner keeps
// read-write (and search) access, which NFS's squashed root needs.
var (
	churnFilePerms = []uint32{0o644, 0o600, 0o640, 0o664}
	churnDirPerms  = []uint32{0o755, 0o700, 0o750}
)

// Symlink targets all start with churnNowhere, a name no path uses, so
// every symlink dangles and resolving one always fails with ENOENT.
const churnNowhere = "gb-none"

// errReroll makes churner.step draw another operation, when the one drawn
// would test behavior the model leaves open (linking a symlink, reading a
// directory).
var errReroll = errors.New("reroll")

// centry is the model of one file, directory or symlink. Hard links share
// one centry.
type centry struct {
	kind     uint32 // unix.S_IFREG, unix.S_IFDIR or unix.S_IFLNK
	perm     uint32
	children map[string]*centry // directories
	target   string             // symlinks
	data     []byte             // regular files
	nlink    int                // regular files
	// mtime is the regular file's mtime as stat showed it right after its
	// last change; every later stat must show exactly this.
	mtime unix.Timespec
	// setTime is whether mtime was set explicitly rather than stamped by a
	// change of content.
	setTime bool
}

// churner runs one worker's seeded operation sequence in its subtree and
// checks every result against its model of what POSIX requires.
type churner struct {
	worker int
	seed   uint64
	rng    *rand.Rand
	root   string  // the subtree's absolute path
	top    *centry // model of root
	start  time.Time
	slack  time.Duration // see mtimeSlack
	ops    int           // operations executed
	desc   string        // the current operation, for failure reports
}

// churnPhase is verify.churn: churnWorkers seeded random operation
// sequences, each in its own subtree of scratch/churn, every result checked
// against a model as it happens and the whole subtree compared with the
// model every churnEvery operations. Untimed afterwards, the subtrees are
// compared again after a cold reset, so everything must have reached
// whatever serves the tree and come back identical, and once more after a
// syncfs, churnLate and another cold reset, to catch late changes.
func (r *runner) churnPhase() phase {
	dir := filepath.Join(r.scratch(), "churn")
	var churners []*churner
	fresh := func() error {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		churners = churners[:0]
		start := time.Now()
		for i := range churnWorkers {
			root := filepath.Join(dir, fmt.Sprintf("w%d", i))
			if err := os.MkdirAll(root, 0o755); err != nil {
				return err
			}
			churners = append(churners, newChurner(i, root, start, r.plan.Kind))
		}
		return nil
	}
	run := func() (result, error) {
		// The model's modes are the ones churn asks for.
		defer unix.Umask(unix.Umask(0))
		err := parallel(churnWorkers, churnWorkers, func(i int) error { return churners[i].churn() })
		var res result
		for _, c := range churners {
			res.ops += int64(c.ops)
		}
		return res, err
	}
	compareAll := func(stage string) error {
		for _, c := range churners {
			if err := c.compare(); err != nil {
				return fmt.Errorf("%s, all %d operations done, %s: %w", c.who(), c.ops, stage, err)
			}
		}
		return nil
	}
	settled := func() error {
		if err := r.coldReset(); err != nil {
			return fmt.Errorf("cold reset: %w", err)
		}
		if err := compareAll("after a cold reset"); err != nil {
			return err
		}
		if err := r.syncTree(); err != nil {
			return err
		}
		time.Sleep(churnLate)
		if err := r.coldReset(); err != nil {
			return fmt.Errorf("cold reset: %w", err)
		}
		if err := compareAll(fmt.Sprintf("after syncfs, %s and a cold reset", churnLate)); err != nil {
			return err
		}
		return os.RemoveAll(dir)
	}
	return phase{name: "verify.churn", before: fresh, body: run, after: settled}
}

// churnWorkerSeed derives worker i's seed from the base seed.
func churnWorkerSeed(i int) uint64 { return churnSeed + uint64(i)*genStream }

// newChurner prepares worker i in the empty directory root.
func newChurner(i int, root string, start time.Time, kind wire.Kind) *churner {
	seed := churnWorkerSeed(i)
	return &churner{
		worker: i,
		seed:   seed,
		rng:    rand.New(rand.NewPCG(seed, genStream)),
		root:   root,
		top:    &centry{kind: unix.S_IFDIR, children: map[string]*centry{}},
		start:  start,
		slack:  mtimeSlack(kind),
	}
}

// mtimeSlack is how far a stamped mtime may lie outside the wall-clock span
// of the run on kind; the exact-value checks never relax.
func mtimeSlack(kind wire.Kind) time.Duration {
	switch kind {
	case wire.KindLocal, wire.KindImage:
		// The guest kernel stamps mtimes from its coarse clock, which lags
		// the fine clock the runner reads by up to a scheduler tick.
		return 50 * time.Millisecond
	default:
		// The host stamps them; the guest's clock was set from the host's
		// at boot, within a millisecond, and drifts over the run.
		return 2 * time.Second
	}
}

// churn runs churnOps operations, comparing the whole subtree with the
// model every churnEvery operations and after the last.
func (c *churner) churn() error {
	for i := range churnOps {
		if err := c.step(); err != nil {
			return fmt.Errorf("%s, operation %d: %s: %w", c.who(), i, c.desc, err)
		}
		c.ops++
		if (i+1)%churnEvery == 0 || i+1 == churnOps {
			if err := c.compare(); err != nil {
				return fmt.Errorf("%s, comparison after operation %d: %w", c.who(), i, err)
			}
		}
	}
	return nil
}

// who names the worker and its seed in failure reports.
func (c *churner) who() string {
	return fmt.Sprintf("worker %d seed %#x (base seed %#x)", c.worker, c.seed, churnSeed)
}

// churnOp is one kind of operation and its share of the mix.
type churnOp struct {
	weight int
	run    func(c *churner) error
}

// churnMix lists the operations and their weights out of 100.
var churnMix = []churnOp{
	{16, (*churner).create},
	{14, (*churner).write},
	{6, (*churner).truncate},
	{12, func(c *churner) error { return c.rename(c.pickPath(0)) }},
	{4, func(c *churner) error { return c.rename(c.pickPath(unix.S_IFDIR)) }},
	{5, (*churner).link},
	{9, (*churner).unlink},
	{9, (*churner).mkdir},
	{6, (*churner).rmdir},
	{4, (*churner).symlink},
	{5, (*churner).chmod},
	{4, (*churner).setMtime},
	{6, (*churner).read},
}

// step draws and runs one operation.
func (c *churner) step() error {
	total := 0
	for _, op := range churnMix {
		total += op.weight
	}
	for {
		n := c.rng.IntN(total)
		for _, op := range churnMix {
			if n < op.weight {
				if err := op.run(c); !errors.Is(err, errReroll) {
					return err
				}
				break
			}
			n -= op.weight
		}
	}
}

// create creates a new file exclusively and writes it by one of three
// patterns.
func (c *churner) create() error {
	p := c.randPath()
	perm := pick(c.rng, churnFilePerms)
	data := c.bytes(c.size())
	style := c.rng.IntN(3)
	c.desc = fmt.Sprintf("create %s mode %#o with %d bytes by %s", rel(p), perm, len(data), writeStyles[style])
	parent, e, want := c.lookup(p)
	if want == 0 && e != nil {
		want = unix.EEXIST
	}
	f, err := c.open(p, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, perm)
	if err := expect(err, want); err != nil || want != 0 {
		closeQuietly(f)
		return err
	}
	if err := c.writeStyled(f, data, style); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	e = &centry{kind: unix.S_IFREG, perm: perm, data: data, nlink: 1}
	parent.children[p[len(p)-1]] = e
	return c.record(p, e)
}

// writeStyles names the ways create writes a new file's content.
var writeStyles = []string{"one write", "several writes", "pwrites in random order"}

// writeStyled writes data to the new file f in style: one write, several
// sequential writes of random sizes, or pwrites of random pieces in random
// order.
func (c *churner) writeStyled(f *os.File, data []byte, style int) error {
	if style == 0 {
		_, err := f.Write(data)
		return err
	}
	var pieces [][2]int
	for off := 0; off < len(data); {
		n := min(len(data)-off, 1+c.rng.IntN(16<<10))
		pieces = append(pieces, [2]int{off, off + n})
		off += n
	}
	if style == 2 {
		c.rng.Shuffle(len(pieces), func(i, j int) { pieces[i], pieces[j] = pieces[j], pieces[i] })
	}
	for _, pc := range pieces {
		var err error
		if style == 1 {
			_, err = f.Write(data[pc[0]:pc[1]])
		} else {
			_, err = f.WriteAt(data[pc[0]:pc[1]], int64(pc[0]))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// write changes an existing file: appends to it, pwrites at an offset that
// may lie past its end (the gap must read as zeros), or truncates it on
// open and writes new content.
func (c *churner) write() error {
	p := c.pickPath(unix.S_IFREG)
	e, want := c.follow(p)
	if want == 0 && e.kind == unix.S_IFDIR {
		want = unix.EISDIR
	}
	style := c.rng.IntN(3)
	flags := unix.O_WRONLY
	var data []byte
	off := 0
	switch style {
	case 0:
		flags |= unix.O_APPEND
		data = c.bytes(c.rng.IntN(16 << 10))
		c.desc = fmt.Sprintf("append %d bytes to %s", len(data), rel(p))
	case 1:
		data = c.bytes(1 + c.rng.IntN(16<<10))
		if want == 0 {
			off = c.rng.IntN(len(e.data) + 8<<10 + 1)
		}
		c.desc = fmt.Sprintf("pwrite %d bytes at %d to %s", len(data), off, rel(p))
	default:
		flags |= unix.O_TRUNC
		data = c.bytes(c.size())
		c.desc = fmt.Sprintf("rewrite %s with %d bytes", rel(p), len(data))
	}
	f, err := c.open(p, flags, 0)
	if err := expect(err, want); err != nil || want != 0 {
		closeQuietly(f)
		return err
	}
	if style == 1 {
		_, err = f.WriteAt(data, int64(off))
	} else {
		_, err = f.Write(data)
	}
	if err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	switch style {
	case 0:
		e.data = slices.Concat(e.data, data)
	case 1:
		next := make([]byte, max(len(e.data), off+len(data)))
		copy(next, e.data)
		copy(next[off:], data)
		e.data = next
	default:
		e.data = data
	}
	e.setTime = false
	return c.record(p, e)
}

// truncate sets an existing file's size, shrinking it or growing it with
// zeros.
func (c *churner) truncate() error {
	p := c.pickPath(unix.S_IFREG)
	e, want := c.follow(p)
	if want == 0 && e.kind == unix.S_IFDIR {
		want = unix.EISDIR
	}
	size := c.rng.IntN(64 << 10)
	if want == 0 {
		size = c.rng.IntN(len(e.data) + 64<<10 + 1)
	}
	c.desc = fmt.Sprintf("truncate %s to %d", rel(p), size)
	if err := expect(unix.Truncate(c.abs(p), int64(size)), want); err != nil || want != 0 {
		return err
	}
	next := make([]byte, size)
	copy(next, e.data)
	e.data = next
	e.setTime = false
	return c.record(p, e)
}

// rename renames src over nothing, a file, a directory, itself or one of
// its own hard links, often into another directory.
func (c *churner) rename(src []string) error {
	var dst []string
	switch n := c.rng.IntN(10); {
	case n == 0:
		dst = src
	case n < 5:
		dst = c.randPath()
	default:
		dst = c.pickPath(0)
	}
	c.desc = fmt.Sprintf("rename %s to %s", rel(src), rel(dst))
	sp, s, want := c.lookup(src)
	var dp, d *centry
	if want == 0 {
		dp, d, want = c.lookup(dst)
	}
	switch {
	case want != 0:
	case s == nil:
		want = unix.ENOENT
	case isStrictPrefix(src, dst):
		// The kernel's rename trap: a directory cannot move below itself.
		want = unix.EINVAL
	case isStrictPrefix(dst, src):
		// The target is an ancestor of the source, so not empty.
		want = unix.ENOTEMPTY
	case s == d:
		// The same path, or two hard links of one file: nothing happens.
	case d == nil:
	case s.kind == unix.S_IFDIR && d.kind != unix.S_IFDIR:
		want = unix.ENOTDIR
	case s.kind != unix.S_IFDIR && d.kind == unix.S_IFDIR:
		want = unix.EISDIR
	case d.kind == unix.S_IFDIR && len(d.children) > 0:
		want = unix.ENOTEMPTY
	}
	if err := expect(unix.Rename(c.abs(src), c.abs(dst)), want); err != nil || want != 0 || s == d {
		return err
	}
	if d != nil && d.kind == unix.S_IFREG {
		d.nlink--
	}
	delete(sp.children, src[len(src)-1])
	dp.children[dst[len(dst)-1]] = s
	return nil
}

// link adds a hard link to an existing file.
func (c *churner) link() error {
	old := c.pickPath(unix.S_IFREG)
	var name []string
	if c.rng.IntN(4) == 0 {
		name = c.pickPath(0)
	} else {
		name = c.randPath()
	}
	_, e, want := c.lookup(old)
	if want == 0 && e == nil {
		want = unix.ENOENT
	}
	if want == 0 && e.kind != unix.S_IFREG {
		return errReroll
	}
	c.desc = fmt.Sprintf("link %s to %s", rel(old), rel(name))
	var parent *centry
	if want == 0 {
		var existing *centry
		parent, existing, want = c.lookup(name)
		if want == 0 && existing != nil {
			want = unix.EEXIST
		}
	}
	if err := expect(unix.Link(c.abs(old), c.abs(name)), want); err != nil || want != 0 {
		return err
	}
	parent.children[name[len(name)-1]] = e
	e.nlink++
	return nil
}

// unlink removes a name, which must not be a directory.
func (c *churner) unlink() error {
	p := c.pickPath(0)
	c.desc = "unlink " + rel(p)
	parent, e, want := c.lookup(p)
	switch {
	case want != 0:
	case e == nil:
		want = unix.ENOENT
	case e.kind == unix.S_IFDIR:
		want = unix.EISDIR
	}
	if err := expect(unix.Unlink(c.abs(p)), want); err != nil || want != 0 {
		return err
	}
	delete(parent.children, p[len(p)-1])
	if e.kind == unix.S_IFREG {
		e.nlink--
	}
	return nil
}

// mkdir creates a directory.
func (c *churner) mkdir() error {
	p := c.randPath()
	perm := pick(c.rng, churnDirPerms)
	c.desc = fmt.Sprintf("mkdir %s mode %#o", rel(p), perm)
	parent, e, want := c.lookup(p)
	if want == 0 && e != nil {
		want = unix.EEXIST
	}
	if err := expect(unix.Mkdir(c.abs(p), perm), want); err != nil || want != 0 {
		return err
	}
	parent.children[p[len(p)-1]] = &centry{kind: unix.S_IFDIR, perm: perm, children: map[string]*centry{}}
	return nil
}

// rmdir removes a directory, which must be empty.
func (c *churner) rmdir() error {
	p := c.pickPath(unix.S_IFDIR)
	c.desc = "rmdir " + rel(p)
	parent, e, want := c.lookup(p)
	switch {
	case want != 0:
	case e == nil:
		want = unix.ENOENT
	case e.kind != unix.S_IFDIR:
		want = unix.ENOTDIR
	case len(e.children) > 0:
		want = unix.ENOTEMPTY
	}
	if err := expect(unix.Rmdir(c.abs(p)), want); err != nil || want != 0 {
		return err
	}
	delete(parent.children, p[len(p)-1])
	return nil
}

// symlink creates a dangling symlink.
func (c *churner) symlink() error {
	p := c.randPath()
	target := fmt.Sprintf("%s/%d", churnNowhere, c.rng.IntN(1000))
	c.desc = fmt.Sprintf("symlink %s to %s", rel(p), target)
	parent, e, want := c.lookup(p)
	if want == 0 && e != nil {
		want = unix.EEXIST
	}
	if err := expect(unix.Symlink(target, c.abs(p)), want); err != nil || want != 0 {
		return err
	}
	parent.children[p[len(p)-1]] = &centry{kind: unix.S_IFLNK, target: target}
	return nil
}

// chmod changes a file's or directory's permission bits; mtime stays.
func (c *churner) chmod() error {
	p := c.pickPath(0)
	e, want := c.follow(p)
	perms := churnFilePerms
	if want == 0 && e.kind == unix.S_IFDIR {
		perms = churnDirPerms
	}
	perm := pick(c.rng, perms)
	c.desc = fmt.Sprintf("chmod %s %#o", rel(p), perm)
	if err := expect(unix.Chmod(c.abs(p), perm), want); err != nil || want != 0 {
		return err
	}
	e.perm = perm
	return nil
}

// setMtime sets a file's atime and mtime to a whole second between 2001 and
// 2031, which every timestamp granularity stores exactly.
func (c *churner) setMtime() error {
	p := c.pickPath(unix.S_IFREG)
	e, want := c.follow(p)
	ts := unix.Timespec{Sec: 978307200 + c.rng.Int64N(30*365*86400)}
	c.desc = fmt.Sprintf("set mtime of %s to %s", rel(p), fmtTimespec(ts))
	err := unix.UtimesNanoAt(unix.AT_FDCWD, c.abs(p), []unix.Timespec{ts, ts}, 0)
	if err := expect(err, want); err != nil || want != 0 || e.kind != unix.S_IFREG {
		return err
	}
	e.mtime = ts
	e.setTime = true
	return c.record(p, e)
}

// read reopens a file and compares its content with the model's.
func (c *churner) read() error {
	p := c.pickPath(unix.S_IFREG)
	e, want := c.follow(p)
	if want == 0 && e.kind == unix.S_IFDIR {
		return errReroll
	}
	c.desc = "read " + rel(p)
	data, err := os.ReadFile(c.abs(p))
	if err := expect(err, want); err != nil || want != 0 {
		return err
	}
	return compareBytes(data, e.data)
}

// record checks a changed file's size and mtime right after the change and
// remembers the mtime: an explicitly set one must read back exactly, a
// stamped one must lie within the run.
func (c *churner) record(p []string, e *centry) error {
	var st unix.Stat_t
	if err := unix.Lstat(c.abs(p), &st); err != nil {
		return fmt.Errorf("lstat after the change: %w", err)
	}
	if st.Size != int64(len(e.data)) {
		return fmt.Errorf("size %d after the change, want %d", st.Size, len(e.data))
	}
	if e.setTime {
		if st.Mtim != e.mtime {
			return fmt.Errorf("mtime %s after setting %s", fmtTimespec(st.Mtim), fmtTimespec(e.mtime))
		}
	} else {
		t := time.Unix(st.Mtim.Unix())
		if t.Before(c.start.Add(-c.slack)) || t.After(time.Now().Add(c.slack)) {
			return fmt.Errorf("mtime %s after the change lies outside the run (started %s, slack %s)",
				fmtTimespec(st.Mtim), c.start.Format(time.RFC3339Nano), c.slack)
		}
	}
	e.mtime = st.Mtim
	return nil
}

// compare checks the whole subtree against the model: each directory's
// names through readdir, and every entry's type, permission bits, symlink
// target, and a regular file's size, link count, mtime and content.
func (c *churner) compare() error { return c.compareDir(nil, c.top) }

// compareDir compares directory p, modelled by dir, and everything below.
func (c *churner) compareDir(p []string, dir *centry) error {
	ents, err := os.ReadDir(c.abs(p))
	if err != nil {
		return err
	}
	names := make([]string, len(ents))
	for i, de := range ents {
		names[i] = de.Name()
	}
	if want := slices.Sorted(maps.Keys(dir.children)); !slices.Equal(names, want) {
		return fmt.Errorf("%s: readdir lists %q, want %q", rel(p), names, want)
	}
	for _, de := range ents {
		cp := append(slices.Clone(p), de.Name())
		e := dir.children[de.Name()]
		if err := c.compareEntry(cp, e, de.Type()); err != nil {
			return fmt.Errorf("%s: %w", rel(cp), err)
		}
		if e.kind == unix.S_IFDIR {
			if err := c.compareDir(cp, e); err != nil {
				return err
			}
		}
	}
	return nil
}

// compareEntry compares one entry with its model; dirType is the type
// readdir reported for it.
func (c *churner) compareEntry(p []string, e *centry, dirType fs.FileMode) error {
	var st unix.Stat_t
	if err := unix.Lstat(c.abs(p), &st); err != nil {
		return fmt.Errorf("lstat: %w", err)
	}
	if got := st.Mode & unix.S_IFMT; got != e.kind {
		return fmt.Errorf("type %#o, want %#o", got, e.kind)
	}
	if want := modeType(e.kind); dirType != want {
		return fmt.Errorf("readdir type %v, want %v", dirType, want)
	}
	if e.kind == unix.S_IFLNK {
		target, err := os.Readlink(c.abs(p))
		if err != nil {
			return err
		}
		if target != e.target {
			return fmt.Errorf("symlink target %q, want %q", target, e.target)
		}
		return nil
	}
	if got := st.Mode & 0o7777; got != e.perm {
		return fmt.Errorf("mode %#o, want %#o", got, e.perm)
	}
	if e.kind == unix.S_IFDIR {
		return nil
	}
	if st.Size != int64(len(e.data)) {
		return fmt.Errorf("size %d, want %d", st.Size, len(e.data))
	}
	if uint64(st.Nlink) != uint64(e.nlink) {
		return fmt.Errorf("link count %d, want %d", st.Nlink, e.nlink)
	}
	if st.Mtim != e.mtime {
		return fmt.Errorf("mtime %s, but stat showed %s after the last change", fmtTimespec(st.Mtim), fmtTimespec(e.mtime))
	}
	data, err := os.ReadFile(c.abs(p))
	if err != nil {
		return err
	}
	return compareBytes(data, e.data)
}

// modeType is the fs.FileMode type bits of a stat file type.
func modeType(kind uint32) fs.FileMode {
	switch kind {
	case unix.S_IFDIR:
		return fs.ModeDir
	case unix.S_IFLNK:
		return fs.ModeSymlink
	}
	return 0
}

// lookup resolves p the way the kernel's path walk does without following a
// final symlink, returning p's parent directory and p's entry (nil when
// absent). A missing component fails with ENOENT, a non-directory followed
// by more components with ENOTDIR, and a symlink component with ENOENT
// because every target dangles.
func (c *churner) lookup(p []string) (parent, e *centry, errno unix.Errno) {
	dir := c.top
	for _, name := range p[:len(p)-1] {
		next := dir.children[name]
		switch {
		case next == nil, next.kind == unix.S_IFLNK:
			return nil, nil, unix.ENOENT
		case next.kind != unix.S_IFDIR:
			return nil, nil, unix.ENOTDIR
		}
		dir = next
	}
	return dir, dir.children[p[len(p)-1]], 0
}

// follow resolves p following a final symlink, as open, truncate, chmod
// and utimensat do; the result is a file or directory, or an errno.
func (c *churner) follow(p []string) (*centry, unix.Errno) {
	_, e, errno := c.lookup(p)
	if errno == 0 && (e == nil || e.kind == unix.S_IFLNK) {
		return nil, unix.ENOENT
	}
	return e, errno
}

// open opens p with flags, returning the file only when the call succeeded.
func (c *churner) open(p []string, flags int, perm uint32) (*os.File, error) {
	fd, err := unix.Open(c.abs(p), flags|unix.O_CLOEXEC, perm)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), c.abs(p)), nil
}

// closeQuietly closes f when it is open: a call that should have failed but
// succeeded leaves an open file behind its mismatch report.
func closeQuietly(f *os.File) {
	if f != nil {
		f.Close()
	}
}

// randPath draws a path of one to three components.
func (c *churner) randPath() []string {
	depth := 1
	switch n := c.rng.IntN(20); {
	case n >= 16:
		depth = 3
	case n >= 7:
		depth = 2
	}
	p := make([]string, depth)
	for i := range p {
		p[i] = pick(c.rng, churnNames)
	}
	return p
}

// pickPath mostly draws an existing path whose entry is of kind (any kind
// for 0), so operations mostly succeed, and otherwise a random path, so
// errors stay common.
func (c *churner) pickPath(kind uint32) []string {
	if c.rng.IntN(4) > 0 {
		var paths [][]string
		walkModel(c.top, nil, func(p []string, e *centry) {
			if kind == 0 || e.kind == kind {
				paths = append(paths, p)
			}
		})
		if len(paths) > 0 {
			return pick(c.rng, paths)
		}
	}
	return c.randPath()
}

// walkModel calls fn for every entry below dir in name order, so draws from
// the walk are deterministic.
func walkModel(dir *centry, prefix []string, fn func(p []string, e *centry)) {
	for _, name := range slices.Sorted(maps.Keys(dir.children)) {
		e := dir.children[name]
		p := append(slices.Clone(prefix), name)
		fn(p, e)
		if e.kind == unix.S_IFDIR {
			walkModel(e, p, fn)
		}
	}
}

// size draws a file size up to 200 KiB, mostly under 8 KiB.
func (c *churner) size() int {
	switch n := c.rng.IntN(10); {
	case n == 0:
		return 0
	case n < 7:
		return 1 + c.rng.IntN(8<<10)
	case n < 9:
		return 8<<10 + c.rng.IntN(56<<10)
	default:
		return 64<<10 + c.rng.IntN(136<<10+1)
	}
}

// bytes draws n bytes of content.
func (c *churner) bytes(n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i += 8 {
		v := c.rng.Uint64()
		for j := i; j < min(i+8, n); j++ {
			b[j] = byte(v)
			v >>= 8
		}
	}
	return b
}

// abs is p's absolute path in the subtree.
func (c *churner) abs(p []string) string {
	return filepath.Join(append([]string{c.root}, p...)...)
}

// rel is p as a slash-separated path for reports.
func rel(p []string) string {
	if len(p) == 0 {
		return "."
	}
	return strings.Join(p, "/")
}

// isStrictPrefix reports whether path a is a proper ancestor of path b.
func isStrictPrefix(a, b []string) bool {
	return len(a) < len(b) && slices.Equal(a, b[:len(a)])
}

// pick draws one element of s.
func pick[T any](r *rand.Rand, s []T) T { return s[r.IntN(len(s))] }

// expect compares a call's error with the errno the model predicts (0 for
// success).
func expect(err error, want unix.Errno) error {
	var got unix.Errno
	if err != nil && !errors.As(err, &got) {
		return fmt.Errorf("got %v, want %s", err, errnoName(want))
	}
	if got != want {
		return fmt.Errorf("got %s, want %s", errnoName(got), errnoName(want))
	}
	return nil
}

// errnoName names an errno for reports; 0 is success.
func errnoName(e unix.Errno) string {
	if e == 0 {
		return "success"
	}
	return unix.ErrnoName(e)
}

// fmtTimespec formats a timestamp to the nanosecond.
func fmtTimespec(ts unix.Timespec) string {
	return time.Unix(ts.Unix()).UTC().Format(time.RFC3339Nano)
}
