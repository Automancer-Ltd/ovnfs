//go:build linux

package main

import (
	"runtime"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// trapCalls is how many statfs calls each trap worker makes.
const trapCalls = 20000

// trapRounds times, in the first round only, statfs on the tree's root in a
// tight loop: an operation no cache answers, so each call is one round trip
// to whatever serves the tree. trap.one runs one worker, trap.par Plan.Jobs
// at once.
func (r *runner) trapRounds() (func(round int) error, error) {
	return func(round int) error {
		if round > 0 {
			return nil
		}
		return r.phases(round, []phase{
			{name: "trap.one", body: func() (result, error) { return statfsLoop(r.root, 1) }},
			{name: "trap.par", body: func() (result, error) { return statfsLoop(r.root, r.plan.Jobs) }},
		})
	}, nil
}

// statfsLoop runs workers goroutines, each locked to its OS thread, making
// trapCalls statfs calls on path. They start together once all are ready;
// the result's duration runs from that start to the last finish, so
// spawning and locking stay untimed, and its Extra holds the merged per-call
// latency percentiles.
func statfsLoop(path string, workers int) (result, error) {
	var (
		ready   sync.WaitGroup
		done    sync.WaitGroup
		start   = make(chan struct{})
		samples = make([][]time.Duration, workers)
		errs    = make([]error, workers)
	)
	for w := range workers {
		samples[w] = make([]time.Duration, 0, trapCalls)
		ready.Add(1)
		done.Go(func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			var st unix.Statfs_t
			ready.Done()
			<-start
			for range trapCalls {
				t := time.Now()
				if err := unix.Statfs(path, &st); err != nil {
					errs[w] = err
					return
				}
				samples[w] = append(samples[w], time.Since(t))
			}
		})
	}
	ready.Wait()
	began := time.Now()
	close(start)
	done.Wait()
	elapsed := time.Since(began)
	for _, err := range errs {
		if err != nil {
			return result{}, err
		}
	}
	all := slices.Concat(samples...)
	slices.Sort(all)
	return result{
		ns:  elapsed.Nanoseconds(),
		ops: int64(len(all)),
		extra: map[string]float64{
			"p50_us": percentileUS(all, 50),
			"p99_us": percentileUS(all, 99),
		},
	}, nil
}

// percentileUS returns the nearest-rank p-th percentile of the sorted
// durations in microseconds.
func percentileUS(sorted []time.Duration, p int) float64 {
	i := max(0, (len(sorted)*p+99)/100-1)
	return float64(sorted[i]) / float64(time.Microsecond)
}
