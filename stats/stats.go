// Package stats keeps a cheap, durable record of what the daemon spends its
// time on, so "why was ovnfs busy at 09:54" still has an answer an hour
// later: which background jobs ran and for how long, how many file calls each
// checkout made, which NFS procedures and admin operations they were, and how
// much CPU the process used meanwhile.
//
// Recording costs the caller two atomic additions ([Timer.Observe]) or one
// short critical section per background job ([Recorder.Job]). Nothing is
// written on those paths: once a minute the recorder appends everything that
// happened since the last time as one batch of JSON lines to the day's
// segment, DIR/2026-11-20.jsonl, without syncing it. A crash loses at most
// that minute, and readers skip a torn last line. Segments older than two
// weeks are deleted.
//
// Each line is one fact about one interval or one job, with durations in
// microseconds:
//
//	{"t":1795164840,"kind":"sample","dt":60,"user_us":5210000,"sys_us":31400000,"goroutines":212}
//	{"t":1795164840,"kind":"timer","dt":60,"group":"nfs","name":"READDIRPLUS","n":85201,"us":20031200}
//	{"t":1795164833,"kind":"job","name":"trash.empty","subject":"caches-c_ad95cafbc115","us":62100000,"cpu_us":41000000}
//
// `ovnfs stats` totals a period ([Recorder.Report]); the segments are plain
// files for anything else (jq, a spreadsheet).
//
//	rec, _ := stats.Open(filepath.Join(state, "stats"), log.Printf)
//	defer rec.Close()
//	lookups := rec.Timer("nfs", "LOOKUP") // once; keep the pointer
//	start := lookups.Start()
//	// ... answer the call ...
//	lookups.Since(start)
//
// A nil *Recorder, *Timer and *Job record nothing, so callers need no check.
package stats

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// flushEvery is how often a batch is appended, and so the resolution of
	// samples and timers.
	flushEvery = time.Minute
	// keepDays is how many daily segments are kept.
	keepDays = 14
	// segmentSuffix follows a segment's date in its file name.
	segmentSuffix = ".jsonl"
)

// The kinds of segment lines.
const (
	kindSample = "sample" // the process over an interval
	kindTimer  = "timer"  // one kind of repeated work over an interval
	kindJob    = "job"    // one run of background work
)

// row is one line of a segment.
type row struct {
	T          int64   `json:"t"`                    // Unix seconds: the end of the interval or job
	Kind       string  `json:"kind"`                 // kindSample, kindTimer or kindJob
	Dt         float64 `json:"dt,omitempty"`         // sample, timer: seconds the interval covers
	UserUs     int64   `json:"user_us,omitempty"`    // sample: process CPU in user mode
	SysUs      int64   `json:"sys_us,omitempty"`     // sample: process CPU in the kernel
	Goroutines int     `json:"goroutines,omitempty"` // sample
	Group      string  `json:"group,omitempty"`      // timer
	Name       string  `json:"name,omitempty"`       // timer, job
	Subject    string  `json:"subject,omitempty"`    // job: what it worked on
	N          int64   `json:"n,omitempty"`          // timer: observations
	Us         int64   `json:"us,omitempty"`         // timer: their total duration; job: its duration
	CPUUs      int64   `json:"cpu_us,omitempty"`     // job: CPU its thread used, where measured
}

// epoch is the origin of Tick.
var epoch = time.Now()

// Tick is a monotonic instant for timing hot paths. Reading it costs half of
// time.Now, which reads the wall clock too.
type Tick int64

// Now returns the current instant.
func Now() Tick { return Tick(time.Since(epoch)) }

// Timer totals the count and duration of one kind of repeated work between
// flushes: an NFS procedure, a checkout's file calls, an admin operation.
type Timer struct {
	n, ns   atomic.Int64
	retired atomic.Bool
}

// Observe adds one piece of work that took d.
func (t *Timer) Observe(d time.Duration) {
	if t != nil {
		t.n.Add(1)
		t.ns.Add(int64(d))
	}
}

// Start returns the instant a piece of work begins at, for Since. A nil timer
// reads no clock.
func (t *Timer) Start() Tick {
	if t == nil {
		return 0
	}
	return Now()
}

// Since adds one piece of work begun at start.
func (t *Timer) Since(start Tick) {
	if t != nil {
		t.Observe(time.Duration(Now() - start))
	}
}

// Retire ends a timer whose subject is gone, such as a removed checkout: the
// next flush writes what it holds and forgets it. [Recorder.Timer] revives it.
func (t *Timer) Retire() {
	if t != nil {
		t.retired.Store(true)
	}
}

// timerKey names a timer.
type timerKey struct{ group, name string }

// Job is one run of background work, from [Recorder.Job] to End.
type Job struct {
	r             *Recorder
	name, subject string
	began         time.Time
	thread        bool          // cpu is measured
	cpu           time.Duration // the thread's CPU time at the start
}

// Recorder aggregates timers and jobs in memory and appends them to the
// segments in its directory; see the package documentation.
type Recorder struct {
	dir  string
	logf func(format string, args ...any)

	// flushing is held across a flush and a report's read of live state and
	// segments, so a report counts every interval once. Taken before mu.
	flushing sync.Mutex
	mu       sync.Mutex
	timers   map[timerKey]*Timer
	running  map[*Job]struct{}
	finished []row     // jobs ended since the last flush
	flushed  time.Time // the end of the last flushed interval
	cpu      cpuTimes  // the process's CPU time then
	// The segment being appended to, which only run's goroutine touches.
	file   *os.File // nil before the first flush
	day    string   // its date, UTC
	failed string   // the last write's error, so a standing one is logged once

	stop chan struct{}
	done chan struct{}
}

// Open starts a recorder that appends to the segments in dir, creating it,
// and deletes segments past their keep. logf, if not nil, reports failed
// writes. It fails when dir cannot be created.
func Open(dir string, logf func(format string, args ...any)) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	now := time.Now()
	r := &Recorder{dir: dir, logf: logf, timers: make(map[timerKey]*Timer), running: make(map[*Job]struct{}), flushed: now, cpu: processCPU(), stop: make(chan struct{}), done: make(chan struct{})}
	r.prune(now)
	go r.run()
	return r, nil
}

// Close appends what the last interval holds and stops the recorder.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	close(r.stop)
	<-r.done
}

// run flushes every flushEvery and once more at Close.
func (r *Recorder) run() {
	defer close(r.done)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			r.flush(time.Now())
			if r.file != nil {
				r.file.Close()
			}
			return
		case now := <-ticker.C:
			r.flush(now)
		}
	}
}

// Timer returns the timer name of group, creating it on first use. It takes a
// lock: hot paths call it once and keep the result.
func (r *Recorder) Timer(group, name string) *Timer {
	if r == nil {
		return nil
	}
	key := timerKey{group, name}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.timers[key]
	if t == nil {
		t = new(Timer)
		r.timers[key] = t
	}
	t.retired.Store(false)
	return t
}

// Job begins one run of the background work name on subject (a snapshot, a
// folder; "" for none). It is reported as running until its End.
func (r *Recorder) Job(name, subject string) *Job { return r.begin(name, subject, false) }

// ThreadJob is Job for work that stays on the calling thread, which the
// caller's goroutine is locked to: End then records the CPU time it used too.
func (r *Recorder) ThreadJob(name, subject string) *Job { return r.begin(name, subject, true) }

// begin registers a running job.
func (r *Recorder) begin(name, subject string, thread bool) *Job {
	if r == nil {
		return nil
	}
	j := &Job{r: r, name: name, subject: subject, began: time.Now(), thread: thread}
	if thread {
		j.cpu = threadCPU()
	}
	r.mu.Lock()
	r.running[j] = struct{}{}
	r.mu.Unlock()
	return j
}

// End records the job's duration. It must be called once.
func (j *Job) End() {
	if j == nil {
		return
	}
	now := time.Now()
	done := row{T: now.Unix(), Kind: kindJob, Name: j.name, Subject: j.subject, Us: now.Sub(j.began).Microseconds()}
	if j.thread {
		done.CPUUs = (threadCPU() - j.cpu).Microseconds()
	}
	j.r.mu.Lock()
	delete(j.r.running, j)
	j.r.finished = append(j.r.finished, done)
	j.r.mu.Unlock()
}

// flush appends the interval that ends now. An interval that cannot be
// written is lost: the next one starts regardless.
func (r *Recorder) flush(now time.Time) {
	r.flushing.Lock()
	defer r.flushing.Unlock()
	r.mu.Lock()
	rows := r.intervalLocked(now, true)
	r.mu.Unlock()
	failed := ""
	if err := r.write(now, rows); err != nil {
		failed = err.Error()
	}
	if failed != "" && failed != r.failed && r.logf != nil {
		r.logf("stats: %s", failed)
	}
	r.failed = failed
}

// intervalLocked returns the rows of the interval since the last flush: the
// process sample, each timer that observed something, in a stable order, and
// the jobs that ended. Draining starts the next interval and forgets retired
// timers; otherwise the interval stays as it is, for a report. r.mu is held.
func (r *Recorder) intervalLocked(now time.Time, drain bool) []row {
	t := now.Unix()
	dt := math.Round(now.Sub(r.flushed).Seconds()*1e3) / 1e3
	used := processCPU()
	rows := []row{{T: t, Kind: kindSample, Dt: dt, UserUs: (used.user - r.cpu.user).Microseconds(), SysUs: (used.sys - r.cpu.sys).Microseconds(), Goroutines: runtime.NumGoroutine()}}
	for key, timer := range r.timers {
		n, ns := timer.n.Load(), timer.ns.Load()
		if drain {
			n, ns = timer.n.Swap(0), timer.ns.Swap(0)
			if timer.retired.Load() {
				delete(r.timers, key)
			}
		}
		if n != 0 {
			rows = append(rows, row{T: t, Kind: kindTimer, Dt: dt, Group: key.group, Name: key.name, N: n, Us: ns / 1e3})
		}
	}
	slices.SortFunc(rows[1:], func(a, b row) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Name, b.Name))
	})
	rows = append(rows, r.finished...)
	if drain {
		r.finished = nil
		r.flushed, r.cpu = now, used
	}
	return rows
}

// write appends rows to the segment of now's day in one write, opening it,
// and deleting segments past their keep, when the day changed.
func (r *Recorder) write(now time.Time, rows []row) error {
	day := now.UTC().Format(time.DateOnly)
	if r.file == nil || day != r.day {
		if r.file != nil {
			r.file.Close()
			r.file = nil
		}
		f, err := openSegment(filepath.Join(r.dir, day+segmentSuffix))
		if err != nil {
			return err
		}
		r.file, r.day = f, day
		r.prune(now)
	}
	var batch bytes.Buffer
	enc := json.NewEncoder(&batch)
	for _, x := range rows {
		if err := enc.Encode(x); err != nil {
			return err
		}
	}
	_, err := r.file.Write(batch.Bytes())
	return err
}

// openSegment opens a segment for appending whole lines: a last line a crash
// cut short is ended, so the first line appended does not join it.
func openSegment(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && info.Size() != 0 {
		var last [1]byte
		if _, err = f.ReadAt(last[:], info.Size()-1); err == nil && last[0] != '\n' {
			_, err = f.Write([]byte{'\n'})
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// prune deletes the segments of days more than keepDays before now.
func (r *Recorder) prune(now time.Time) {
	oldest := now.UTC().AddDate(0, 0, -keepDays).Format(time.DateOnly) + segmentSuffix
	entries, err := os.ReadDir(r.dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) && r.logf != nil {
		r.logf("stats: %v", err)
	}
	for _, e := range entries {
		if name := e.Name(); len(name) == len(oldest) && filepath.Ext(name) == segmentSuffix && name < oldest {
			os.Remove(filepath.Join(r.dir, name))
		}
	}
}
