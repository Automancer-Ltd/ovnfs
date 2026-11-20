//go:build !windows

package kfs

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/overlay"
)

// Workload shape: the small-file phase of the guest benchmark.
const (
	workloadWorkers = 4 // KFS_WORKLOAD_WORKERS overrides it
	workloadDirs    = 100
	workloadFiles   = 10000
	workloadLinks   = 1000
)

// opTimes collects the latency of each op a workload runs.
type opTimes struct {
	mu  sync.Mutex
	ops map[string][]time.Duration
	// calls counts the checkout's file calls per phase, by its first op.
	calls map[string]int64
}

// time runs fn and records its duration under op, failing t on error.
func (o *opTimes) time(t *testing.T, op string, fn func() error) {
	start := time.Now()
	err := fn()
	took := time.Since(start)
	if err != nil {
		t.Errorf("%s: %v", op, err)
		return
	}
	o.mu.Lock()
	o.ops[op] = append(o.ops[op], took)
	o.mu.Unlock()
}

// report logs count, mean and median per op.
func (o *opTimes) report(t *testing.T) {
	order := []string{"MKDIR", "CREATE", "WRITE", "READ", "LINK", "FLINK", "FREAD", "BLINK", "BREAD", "RENAME", "UNLINK", "RMDIR"}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-7s %7s %10s %10s %11s", "op", "calls", "mean", "median", "file calls")
	for _, op := range order {
		d := o.ops[op]
		if len(d) == 0 {
			continue
		}
		var sum time.Duration
		for _, x := range d {
			sum += x
		}
		slices.Sort(d)
		fmt.Fprintf(&b, "\n%-7s %7d %10v %10v %11.2f", op, len(d), (sum / time.Duration(len(d))).Round(100*time.Nanosecond), d[len(d)/2].Round(100*time.Nanosecond), float64(o.calls[op])/float64(len(d)))
	}
	t.Log(b.String())
}

// TestMutationWorkload drives the guest benchmark's small-file workload over
// four connections: 10,000 1 KiB files in 100 directories created and
// written, read back after the memory tier let them go, linked, renamed and
// removed. It runs only with KFS_WORKLOAD set, against the kernel share at
// KFS_WORKLOAD_SOCK (a daemon's; its checkout must hold an empty directory
// named by KFS_WORKLOAD_DIR, default "w") or else an in-process fixture.
func TestMutationWorkload(t *testing.T) {
	if os.Getenv("KFS_WORKLOAD") == "" {
		t.Skip("set KFS_WORKLOAD to run")
	}
	sock := os.Getenv("KFS_WORKLOAD_SOCK")
	// calls counts the checkout's file calls in-process; a daemon's are its own.
	calls := func() int64 { return 0 }
	if sock == "" {
		// The daemon's memory tier: files leave it after 2 s idle.
		f := newFixtureWith(t, overlay.NewMemory(64<<20, 256<<10, 2*time.Second), "vm")
		sock = f.serve(t, "vm", true)
		calls = func() int64 {
			r, err := f.stats.Report(t.Context(), api.StatsArgs{})
			if err != nil {
				t.Fatal(err)
			}
			var n int64
			for _, x := range r.Timers {
				if x.Group == "calls" {
					n += x.Count
				}
			}
			return n
		}
	}
	workers := workloadWorkers
	if n, err := strconv.Atoi(os.Getenv("KFS_WORKLOAD_WORKERS")); err == nil && n > 0 {
		workers = n
	}
	conns := make([]*Client, workers)
	for i := range conns {
		conns[i] = dial(t, sock)
	}
	top := must(conns[0].Mkdir(RootID, cmp.Or(os.Getenv("KFS_WORKLOAD_DIR"), "w"), 0o755))(t)
	times := &opTimes{ops: map[string][]time.Duration{}, calls: map[string]int64{}}
	// each runs fn(c, i) for every i below n, item i on connection i%workers,
	// and counts the file calls it made under op.
	each := func(op string, n int, fn func(c *Client, i int)) {
		before := calls()
		defer func() { times.calls[op] += calls() - before }()
		var wg sync.WaitGroup
		for w, c := range conns {
			wg.Go(func() {
				for i := w; i < n; i += workers {
					fn(c, i)
				}
			})
		}
		wg.Wait()
	}
	dirs := make([]uint64, workloadDirs)
	each("MKDIR", workloadDirs, func(c *Client, i int) {
		times.time(t, "MKDIR", func() error {
			a, err := c.Mkdir(top.ID, fmt.Sprintf("d%03d", i), 0o755)
			dirs[i] = a.ID
			return err
		})
	})
	files := make([]uint64, workloadFiles)
	data := make([]byte, 1024)
	small, big := make([]byte, 16<<10), make([]byte, 300<<10)
	name := func(i int) string { return fmt.Sprintf("f%05d", i) }
	each("CREATE", workloadFiles, func(c *Client, i int) {
		times.time(t, "CREATE", func() error {
			a, err := c.Create(dirs[i%workloadDirs], name(i), 0o644, CreateExcl)
			files[i] = a.ID
			return err
		})
		times.time(t, "WRITE", func() error { _, err := c.Write(files[i], 0, data); return err })
	})
	time.Sleep(3 * time.Second)
	each("READ", workloadFiles, func(c *Client, i int) {
		times.time(t, "READ", func() error {
			got, err := c.Read(files[i], 0, 4096)
			if err == nil && len(got) != len(data) {
				err = fmt.Errorf("read %d bytes", len(got))
			}
			return err
		})
	})
	each("LINK", workloadLinks, func(c *Client, i int) {
		times.time(t, "LINK", func() error { _, err := c.Link(files[i], dirs[i%workloadDirs], name(i)+".l"); return err })
	})
	// Freshly written files, small ones in memory and ones past the memory
	// tier's per-file limit on disk, hard-linked and read back right away, as
	// a compiler's output is.
	each("", workloadLinks, func(c *Client, i int) {
		data := small
		if i%2 == 1 {
			data = big
		}
		a, err := c.Create(dirs[i%workloadDirs], name(i)+".o", 0o644, CreateExcl)
		if err == nil {
			_, err = c.Write(a.ID, 0, data)
		}
		if err != nil {
			t.Error(err)
			return
		}
		op := "FLINK"
		if i%2 == 1 {
			op = "BLINK"
		}
		times.time(t, op, func() error { _, err := c.Link(a.ID, dirs[i%workloadDirs], name(i)+".ol"); return err })
		times.time(t, op[:1]+"READ", func() error {
			got, err := c.Read(a.ID, 0, 4096)
			if err == nil && len(got) == 0 {
				err = fmt.Errorf("read %d bytes", len(got))
			}
			return err
		})
	})
	each("", workloadLinks, func(c *Client, i int) {
		for _, suffix := range []string{".l", ".o", ".ol"} {
			if err := c.Unlink(dirs[i%workloadDirs], name(i)+suffix); err != nil {
				t.Error(err)
			}
		}
	})
	each("RENAME", workloadFiles, func(c *Client, i int) {
		times.time(t, "RENAME", func() error {
			return c.Rename(dirs[i%workloadDirs], name(i), dirs[i%workloadDirs], name(i)+".r", 0)
		})
	})
	each("UNLINK", workloadFiles, func(c *Client, i int) {
		times.time(t, "UNLINK", func() error { return c.Unlink(dirs[i%workloadDirs], name(i)+".r") })
	})
	each("RMDIR", workloadDirs, func(c *Client, i int) {
		times.time(t, "RMDIR", func() error { return c.Rmdir(top.ID, fmt.Sprintf("d%03d", i)) })
	})
	if err := conns[0].Rmdir(RootID, cmp.Or(os.Getenv("KFS_WORKLOAD_DIR"), "w")); err != nil {
		t.Error(err)
	}
	times.report(t)
}

// TestCompoundCreates measures what one connection's COMPOUNDs of 256
// requests cost the daemon per file: creates with a guest id and 4 KiB of
// content in one CREATE, the same as CREATE + WRITE + SETATTR, and renames
// and unlinks of the files while the memory tier holds them, 20,000 files
// each. It runs with KFS_WORKLOAD set, like [TestMutationWorkload].
func TestCompoundCreates(t *testing.T) {
	if os.Getenv("KFS_WORKLOAD") == "" {
		t.Skip("set KFS_WORKLOAD to run")
	}
	sock := os.Getenv("KFS_WORKLOAD_SOCK")
	if sock == "" {
		budget := int64(64 << 20)
		if v, err := strconv.ParseInt(os.Getenv("KFS_WORKLOAD_MEMORY"), 10, 64); err == nil {
			budget = v << 20 // MiB
		}
		f := newFixtureWith(t, overlay.NewMemory(budget, 256<<10, 2*time.Second), "vm")
		sock = f.serve(t, "vm", true)
	}
	c := dial(t, sock)
	const files, batch = 20000, 256
	top := must(c.Mkdir(RootID, cmp.Or(os.Getenv("KFS_WORKLOAD_DIR"), "w")+"-c", 0o755))(t)
	data := make([]byte, 4096)
	mtime := uint64(time.Now().UnixNano())
	set := SetattrReq{Valid: SetMtime, MtimeNs: mtime}
	var b strings.Builder
	report := func(form string, took time.Duration) {
		fmt.Fprintf(&b, "\n%-28s %8.2f µs per file", form, float64(took.Microseconds())/files)
	}
	// run sends files requests in COMPOUNDs of batch sub-requests each, the
	// ones for file i from reqs(i), and returns the time it took.
	run := func(per int, reqs func(i int) [][]byte) time.Duration {
		start := time.Now()
		for i := 0; i < files; {
			var subs [][]byte
			size := ReqSize
			for ; i < files && len(subs)+per <= batch; i++ {
				next := reqs(i)
				n := 0
				for _, r := range next {
					n += subHeader + len(r)
				}
				if size+n > MaxFrame {
					break // 256 creates with 4 KiB each do not fit one frame
				}
				subs, size = append(subs, next...), size+n
			}
			replies := must(c.Compound(subs...))(t)
			for _, r := range replies {
				if r.Err != nil {
					t.Fatalf("sub-request: %v", r.Err)
				}
			}
		}
		return time.Since(start)
	}
	// ids reserves an id per file and a directory per batch.
	ids := func() (fileIDs []uint64, dirs []uint64) {
		for len(fileIDs) < files {
			first, n, err := c.AllocIDs(uint32(files - len(fileIDs)))
			if err != nil {
				t.Fatal(err)
			}
			for k := range uint64(n) {
				fileIDs = append(fileIDs, first+k)
			}
		}
		for i := 0; i < files; i += batch {
			d := must(c.Mkdir(top.ID, fmt.Sprintf("d%d", len(dirs)+int(fileIDs[0]%1000000)), 0o755))(t)
			dirs = append(dirs, d.ID)
		}
		return fileIDs, dirs
	}
	name := func(i int) []byte { return fmt.Appendf(nil, "f%05d", i) }

	oneIDs, oneDirs := ids()
	report("CREATE with content", run(1, func(i int) [][]byte {
		return [][]byte{c.Request(OpCreate, oneDirs[i/batch], c.CreateBody(string(name(i)), 0o644, CreateExcl, oneIDs[i], mtime, data)...)}
	}))
	report("RENAME", run(1, func(i int) [][]byte {
		r := RenameReq{NewDir: oneDirs[i/batch], OldLen: 6, NewLen: 8}
		return [][]byte{c.Request(OpRename, oneDirs[i/batch], r.append(nil), name(i), append(name(i), ".r"...))}
	}))
	report("UNLINK", run(1, func(i int) [][]byte {
		return [][]byte{c.Request(OpUnlink, oneDirs[i/batch], append(name(i), ".r"...))}
	}))
	threeIDs, threeDirs := ids()
	report("CREATE + WRITE + SETATTR", run(3, func(i int) [][]byte {
		return [][]byte{
			c.Request(OpCreate, threeDirs[i/batch], c.CreateBody(string(name(i)), 0o644, CreateExcl, threeIDs[i], mtime, nil)...),
			c.Request(OpWrite, threeIDs[i], writeBody(0, data)),
			c.Request(OpSetattr, threeIDs[i], set.append(nil)),
		}
	}))
	t.Log(b.String())
}
