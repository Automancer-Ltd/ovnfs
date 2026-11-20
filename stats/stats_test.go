package stats

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// open starts a recorder on a fresh directory; the test flushes it by hand.
func open(t *testing.T) *Recorder {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "stats"), t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

// report totals the last hour.
func report(t *testing.T, r *Recorder) api.StatsReply {
	t.Helper()
	reply, err := r.Report(t.Context(), api.StatsArgs{})
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

// timer returns the total of group and name in reply; zero when absent.
func timer(reply api.StatsReply, group, name string) api.StatsTimer {
	for _, x := range reply.Timers {
		if x.Group == group && x.Name == name {
			return x
		}
	}
	return api.StatsTimer{}
}

// TestReportCountsEachIntervalOnce checks that a report totals what is in
// memory, what was flushed and what a reopened recorder finds on disk, each
// exactly once, across the flush that moves it from memory to a segment.
func TestReportCountsEachIntervalOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stats")
	r, err := Open(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	lookups := r.Timer("nfs", "LOOKUP")
	lookups.Observe(2 * time.Second)
	lookups.Observe(time.Second)
	running := r.Job("snapshot.take", "c_1 of /src")
	r.Job("trash.empty", "caches-c_0").End()

	check := func(when string, r *Recorder, wantRunning int) {
		t.Helper()
		reply := report(t, r)
		if got := timer(reply, "nfs", "LOOKUP"); got.Count != 2 || got.Seconds != 3 {
			t.Errorf("%s: LOOKUP = %d calls, %v s; want 2 calls, 3 s", when, got.Count, got.Seconds)
		}
		if len(reply.Jobs) != 1 || reply.Jobs[0].Name != "trash.empty" || reply.Jobs[0].Runs != 1 || reply.Jobs[0].Longest != "caches-c_0" {
			t.Errorf("%s: jobs = %+v; want one trash.empty run on caches-c_0", when, reply.Jobs)
		}
		if len(reply.Running) != wantRunning {
			t.Errorf("%s: running = %+v; want %d", when, reply.Running, wantRunning)
		}
	}
	check("unflushed", r, 1)
	r.flush(time.Now())
	check("flushed", r, 1)
	running.End()
	r.Close()

	again, err := Open(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	reply := report(t, again)
	if got := timer(reply, "nfs", "LOOKUP"); got.Count != 2 || got.Seconds != 3 {
		t.Errorf("reopened: LOOKUP = %d calls, %v s; want 2 calls, 3 s", got.Count, got.Seconds)
	}
	if len(reply.Jobs) != 2 || len(reply.Running) != 0 {
		t.Errorf("reopened: jobs = %+v, running = %+v; want both finished jobs and none running", reply.Jobs, reply.Running)
	}
}

// TestReportSpansDaysWithinItsPeriod checks that a report reads every day's
// segment its period touches and leaves out what ended before the period.
func TestReportSpansDaysWithinItsPeriod(t *testing.T) {
	r := open(t)
	calls := r.Timer("calls", "app")
	now := time.Now()
	for _, age := range []time.Duration{50 * time.Hour, 26 * time.Hour, 30 * time.Minute} {
		calls.Observe(time.Second)
		r.flush(now.Add(-age))
	}
	segments, err := os.ReadDir(r.dir)
	if err != nil || len(segments) < 2 {
		t.Fatalf("segments = %v, %v; want one a day", segments, err)
	}
	for since, want := range map[time.Duration]int64{time.Hour: 1, 48 * time.Hour: 2, 72 * time.Hour: 3} {
		reply, err := r.Report(t.Context(), api.StatsArgs{Since: since.Seconds()})
		if err != nil {
			t.Fatal(err)
		}
		if got := timer(reply, "calls", "app").Count; got != want {
			t.Errorf("last %v: %d calls, want %d", since, got, want)
		}
	}
}

// TestTornLineCostsOnlyItself checks that a line a crash cut short is skipped
// and that the next batch starts a line of its own instead of joining it.
func TestTornLineCostsOnlyItself(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at := strconv.FormatInt(now.Unix(), 10)
	whole := `{"t":` + at + `,"kind":"timer","dt":60,"group":"nfs","name":"READ","n":5,"us":1000000}`
	torn := `{"t":` + at + `,"kind":"timer","dt":60,"group":"nfs","name":"READ","n":`
	segment := filepath.Join(dir, now.UTC().Format(time.DateOnly)+segmentSuffix)
	if err := os.WriteFile(segment, []byte(whole+"\n"+torn), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	r.Timer("nfs", "READ").Observe(time.Second)
	r.flush(now)
	if got := timer(report(t, r), "nfs", "READ"); got.Count != 6 || got.Seconds != 2 {
		t.Errorf("READ = %d calls, %v s; want the whole line's 5 and the new 1, 2 s", got.Count, got.Seconds)
	}
}

// TestOpenDeletesSegmentsPastTheirKeep checks that only segments older than
// keepDays go, and nothing that is no segment.
func TestOpenDeletesSegmentsPastTheirKeep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stats")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	day := func(ago int) string {
		return time.Now().UTC().AddDate(0, 0, -ago).Format(time.DateOnly) + segmentSuffix
	}
	for _, name := range []string{day(keepDays + 1), day(keepDays), day(0), "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Open(dir, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got, want := strings.Join(left, " "), strings.Join([]string{day(keepDays), day(0), "notes.txt"}, " "); got != want {
		t.Errorf("left %q, want %q", got, want)
	}
}

// TestRetiredTimerIsWrittenThenForgotten checks that a retired timer's last
// observations reach the segment, that the recorder then drops it, and that
// asking for it again starts a timer that counts.
func TestRetiredTimerIsWrittenThenForgotten(t *testing.T) {
	r := open(t)
	gone := r.Timer("calls", "gone")
	gone.Observe(time.Second)
	gone.Retire()
	r.flush(time.Now())
	r.mu.Lock()
	kept := len(r.timers)
	r.mu.Unlock()
	if kept != 0 {
		t.Errorf("%d timers kept after the retired one was flushed", kept)
	}
	r.Timer("calls", "gone").Observe(time.Second)
	if got := timer(report(t, r), "calls", "gone"); got.Count != 2 {
		t.Errorf("gone = %d calls; want the retired timer's 1 and its successor's 1", got.Count)
	}
}

// TestNilRecorderRecordsNothing checks the nil forms every caller relies on.
func TestNilRecorderRecordsNothing(t *testing.T) {
	var r *Recorder
	timer := r.Timer("nfs", "READ")
	timer.Since(timer.Start())
	timer.Retire()
	r.Job("store.sweep", "").End()
	r.ThreadJob("trash.empty", "x").End()
	r.Close()
	if _, err := r.Report(t.Context(), api.StatsArgs{}); err == nil {
		t.Error("a nil recorder reported")
	}
}
