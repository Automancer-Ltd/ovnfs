package stats

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// defaultPeriod is the period a report covers when the caller names none.
const defaultPeriod = time.Hour

// Report totals the last a.Since seconds, an hour by default: what the
// segments hold of them plus the interval not flushed yet, and the jobs still
// running. The admin operation "stats" calls it. It fails when the daemon
// records nothing (r is nil) or a segment cannot be read.
func (r *Recorder) Report(_ context.Context, a api.StatsArgs) (api.StatsReply, error) {
	if r == nil {
		return api.StatsReply{}, errors.New("stats: the daemon records none; its log says why")
	}
	now := time.Now()
	period := defaultPeriod
	if a.Since > 0 {
		period = time.Duration(a.Since * float64(time.Second))
	}
	from := now.Add(-period)
	t := totals{from: from.Unix(), timers: make(map[timerKey]*api.StatsTimer), jobs: make(map[string]*api.StatsJob)}
	t.reply.From, t.reply.To = from, now

	r.flushing.Lock()
	defer r.flushing.Unlock()
	// No segment is older than its keep, however long the period asked for.
	last := now.UTC().Format(time.DateOnly)
	for day := maxTime(from, now.AddDate(0, 0, -keepDays)).UTC(); day.Format(time.DateOnly) <= last; day = day.AddDate(0, 0, 1) {
		if err := readSegment(filepath.Join(r.dir, day.Format(time.DateOnly)+segmentSuffix), t.add); err != nil {
			return api.StatsReply{}, err
		}
	}
	r.mu.Lock()
	for _, x := range r.intervalLocked(now, false) {
		t.add(x)
	}
	for j := range r.running {
		t.reply.Running = append(t.reply.Running, api.StatsRun{Name: j.name, Subject: j.subject, Seconds: now.Sub(j.began).Seconds()})
	}
	r.mu.Unlock()
	return t.finish(), nil
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// readSegment hands each line of the segment at path to add. A segment that
// does not exist holds nothing, and a line that does not parse is skipped: a
// crash may cut the last one short.
func readSegment(path string, add func(row)) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		var x row
		if json.Unmarshal(lines.Bytes(), &x) == nil {
			add(x)
		}
	}
	return lines.Err()
}

// totals accumulates a report from rows.
type totals struct {
	from   int64 // rows ending at or before it are outside the period
	reply  api.StatsReply
	timers map[timerKey]*api.StatsTimer
	jobs   map[string]*api.StatsJob
}

// add counts one row.
func (t *totals) add(x row) {
	if x.T <= t.from {
		return
	}
	switch x.Kind {
	case kindSample:
		used := float64(x.UserUs+x.SysUs) / 1e6
		t.reply.Recorded += x.Dt
		t.reply.CPU.User += float64(x.UserUs) / 1e6
		t.reply.CPU.Sys += float64(x.SysUs) / 1e6
		// A sample of under a second (the last before a stop) is too short to rank.
		if x.Dt >= 1 && used/x.Dt > t.reply.CPU.Peak {
			t.reply.CPU.Peak, t.reply.CPU.PeakAt = used/x.Dt, time.Unix(x.T, 0)
		}
	case kindTimer:
		key := timerKey{x.Group, x.Name}
		total := t.timers[key]
		if total == nil {
			total = &api.StatsTimer{Group: x.Group, Name: x.Name}
			t.timers[key] = total
		}
		total.Count += x.N
		total.Seconds += float64(x.Us) / 1e6
	case kindJob:
		total := t.jobs[x.Name]
		if total == nil {
			total = &api.StatsJob{Name: x.Name}
			t.jobs[x.Name] = total
		}
		seconds := float64(x.Us) / 1e6
		total.Runs++
		total.Seconds += seconds
		total.CPU += float64(x.CPUUs) / 1e6
		if total.Runs == 1 || seconds > total.LongestSeconds {
			total.Longest, total.LongestSeconds = x.Subject, seconds
		}
	}
}

// finish orders the totals: jobs and running jobs by time, timers by group,
// then time.
func (t *totals) finish() api.StatsReply {
	// Never nil: an empty period reports [], not null.
	t.reply.Jobs = make([]api.StatsJob, 0, len(t.jobs))
	if t.reply.Running == nil {
		t.reply.Running = []api.StatsRun{}
	}
	t.reply.Timers = make([]api.StatsTimer, 0, len(t.timers))
	for _, job := range t.jobs {
		t.reply.Jobs = append(t.reply.Jobs, *job)
	}
	slices.SortFunc(t.reply.Jobs, func(a, b api.StatsJob) int {
		return cmp.Or(cmp.Compare(b.Seconds, a.Seconds), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(t.reply.Running, func(a, b api.StatsRun) int {
		return cmp.Or(cmp.Compare(b.Seconds, a.Seconds), cmp.Compare(a.Name, b.Name), cmp.Compare(a.Subject, b.Subject))
	})
	for _, timer := range t.timers {
		t.reply.Timers = append(t.reply.Timers, *timer)
	}
	slices.SortFunc(t.reply.Timers, func(a, b api.StatsTimer) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(b.Seconds, a.Seconds), cmp.Compare(a.Name, b.Name))
	})
	return t.reply
}
