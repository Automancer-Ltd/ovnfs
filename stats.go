package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/api"
)

// statsCmd totals what the daemon spent a period on, from the record it keeps
// below STATE/stats (package stats): its CPU time, its background jobs and
// the file calls, NFS procedures and admin operations it answered. With
// --json the reply (api.StatsReply) is printed raw.
type statsCmd struct {
	Since seconds `name:"since" help:"total this long back from now: 10m, 12h, 7d (default 1h)"`
}

func (cmd *statsCmd) Run(c *cli) error {
	reply, err := call[api.StatsReply](c, "stats", api.StatsArgs{Since: float64(cmd.Since)})
	if err != nil || c.json {
		return err
	}
	printStats(stdout, reply)
	return nil
}

// statsGroup heads the table of one timer group (api.StatsTimer.Group).
type statsGroup struct{ group, title string }

// statsGroups are the groups the daemon records, in the order they print;
// another group prints last under its own name.
var statsGroups = []statsGroup{
	{"calls", "CHECKOUT"},
	{"nfs", "NFS"},
	{"admin", "ADMIN"},
}

// printStats lays a stats reply out: the period and CPU time as labelled
// rows, then a table of jobs and one of each timer group.
func printStats(w io.Writer, r api.StatsReply) {
	cpu := r.CPU.User + r.CPU.Sys
	rows := [][2]string{
		{"period", fmt.Sprintf("%s to %s (%s, %s recorded)", r.From.Local().Format("Jan 2 15:04"), r.To.Local().Format("15:04"), span(r.To.Sub(r.From).Seconds()), span(r.Recorded))},
		{"cpu", fmt.Sprintf("%s: %s user, %s system", span(cpu), span(r.CPU.User), span(r.CPU.Sys))},
	}
	if r.Recorded > 0 {
		rows[1][1] += fmt.Sprintf("; %s of a core on average", percent(cpu/r.Recorded))
	}
	if !r.CPU.PeakAt.IsZero() {
		rows[1][1] += fmt.Sprintf(", %s in the busiest minute (%s)", percent(r.CPU.Peak), r.CPU.PeakAt.Local().Format("15:04"))
	}
	for _, run := range r.Running {
		rows = append(rows, [2]string{"running", fmt.Sprintf("%s %s for %s", run.Name, run.Subject, span(run.Seconds))})
	}
	labelled(w, rows)

	if len(r.Jobs) != 0 {
		jobs := [][]string{bold(w, "JOB", "RUNS", "TIME", "CPU", "LONGEST")}
		for _, job := range r.Jobs {
			used := paint(w, styleDim, "-")
			if job.CPU > 0 {
				used = span(job.CPU)
			}
			jobs = append(jobs, []string{job.Name, commas(int64(job.Runs)), span(job.Seconds), used, strings.TrimSpace(span(job.LongestSeconds) + " " + job.Longest)})
		}
		fmt.Fprintln(w)
		table(w, jobs)
	}
	groups := slices.Clone(statsGroups)
	for _, timer := range r.Timers {
		if !slices.ContainsFunc(groups, func(g statsGroup) bool { return g.group == timer.Group }) {
			groups = append(groups, statsGroup{timer.Group, strings.ToUpper(timer.Group)})
		}
	}
	for _, g := range groups {
		timers := [][]string{bold(w, g.title, "CALLS", "TIME")}
		for _, timer := range r.Timers {
			if timer.Group == g.group {
				timers = append(timers, []string{timer.Name, commas(timer.Count), span(timer.Seconds)})
			}
		}
		if len(timers) > 1 {
			fmt.Fprintln(w)
			table(w, timers)
		}
	}
}

// bold styles a table's header cells.
func bold(w io.Writer, cells ...string) []string {
	for i, cell := range cells {
		cells[i] = paint(w, styleBold, cell)
	}
	return cells
}

// span renders a duration in seconds at the precision its size merits:
// 40µs, 350ms, 3.4s, 2m10s, 3h5m.
func span(seconds float64) string {
	d := time.Duration(seconds * float64(time.Second))
	switch {
	case d < time.Millisecond:
		return strconv.FormatInt(d.Microseconds(), 10) + "µs"
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < time.Minute:
		return strconv.FormatFloat(seconds, 'f', 1, 64) + "s"
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%ds", d/time.Minute, d%time.Minute/time.Second)
	}
	d = d.Round(time.Minute)
	return fmt.Sprintf("%dh%dm", d/time.Hour, d%time.Hour/time.Minute)
}

// percent renders a share of one core.
func percent(cores float64) string {
	return strconv.FormatFloat(cores*100, 'f', 0, 64) + "%"
}
