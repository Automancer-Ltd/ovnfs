//go:build linux

// Command guest is guestbench's runner inside a vibevmm microVM.
//
// The rootfs's /init mounts the bench share at wire.Dir and execs this binary
// as PID 1. It reads the plan, brings up the network and the tree under test,
// runs each listed workload in wire.Workloads order for every round, and
// reports each timed phase as a wire.Event line on the console. A workload
// finishes all its rounds before the next starts, so cargo's target directory
// carries from one cargo round to the next while no other workload ever sees
// it. However the run ends it syncs, unmounts and powers the VM off: PID 1
// must never exit.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// env is the environment the rootfs's tools need; /init exports only PATH,
// and rustup cannot pick a toolchain without RUSTUP_HOME.
var env = [][2]string{
	{"PATH", "/usr/local/cargo/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	{"HOME", "/root"},
	{"RUSTUP_HOME", "/usr/local/rustup"},
	{"CARGO_HOME", "/usr/local/cargo"},
}

// outputTail is how many trailing bytes of a failed command's output its
// error carries.
const outputTail = 2000

// runner is one boot's state: the plan, the tree under test and what earlier
// workloads learned about it.
type runner struct {
	plan    wire.Plan
	root    string // the tree under test
	mounted bool   // whether wire.Mount holds the tree
	lower   string // KindImage's loop device holding the image
	files   int64  // regular files, as the last walk counted them
}

// workloads maps each name of wire.Workloads to its set-up, which runs once
// before the workload's rounds and returns the function running one round.
var workloads = map[string]func(*runner) (func(round int) error, error){
	wire.Net:    (*runner).netRounds,
	wire.Walk:   (*runner).walkRounds,
	wire.Git:    (*runner).gitRounds,
	wire.Rg:     (*runner).rgRounds,
	wire.Fio:    (*runner).fioRounds,
	wire.Small:  (*runner).smallRounds,
	wire.Replay: (*runner).replayRounds,
	wire.Cargo:  (*runner).cargoRounds,
	wire.Verify: (*runner).verifyRounds,
}

// main runs the plan as PID 1, reports a failure as the last event and
// powers the VM off however the run ended. A KindModule failure carries the
// kernel log's tail, where the module reports load errors and its bugs.
func main() {
	reapOrphans()
	r := &runner{}
	if err := r.guarded(); err != nil {
		msg := err.Error()
		if r.plan.Kind == wire.KindModule {
			msg += "\nkernel log:\n" + kernelLog()
		}
		emit(wire.Event{Ev: wire.EvFatal, Msg: msg})
	}
	r.powerOff()
}

// guarded runs the plan and turns a panic of the main goroutine into an
// error, so it is reported before the power-off instead of killing PID 1.
func (r *runner) guarded() (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return r.bench()
}

// bench runs the whole plan: set-up, every listed workload's rounds and the
// final cleanup.
func (r *runner) bench() error {
	for _, kv := range env {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			return err
		}
	}
	emit(wire.Event{Ev: wire.EvBoot})
	plan, err := loadPlan()
	if err != nil {
		return err
	}
	r.plan = plan
	if plan.Net != nil {
		if err := configureNet(plan.Net); err != nil {
			return fmt.Errorf("network: %w", err)
		}
	}
	// Set before any file is written, so every mtime is the host's era.
	if plan.Echo != "" {
		if err := syncClock(plan.Echo); err != nil {
			return fmt.Errorf("clock: %w", err)
		}
	}
	if err := r.prepareTree(); err != nil {
		return fmt.Errorf("tree: %w", err)
	}
	for _, w := range wire.Workloads {
		if !slices.Contains(plan.Workloads, w) {
			continue
		}
		round, err := workloads[w](r)
		if err != nil {
			return fmt.Errorf("%s: %w", w, err)
		}
		for i := range plan.Rounds {
			if err := round(i); err != nil {
				return fmt.Errorf("round %d: %w", i, err)
			}
		}
	}
	if err := errors.Join(r.removeTarget(), os.RemoveAll(r.scratch())); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	done := wire.Event{Ev: wire.EvDone}
	if plan.Kind == wire.KindModule {
		log := " clean"
		if w := kernelWarnings(); w != "" {
			log = "\n" + w
		}
		done.Msg = moduleCalls() + "\nkernel log:" + log
	}
	emit(done)
	return nil
}

// loadPlan reads the host's plan from the bench share and rejects one the
// runner would otherwise silently misread.
func loadPlan() (wire.Plan, error) {
	var p wire.Plan
	data, err := os.ReadFile(filepath.Join(wire.Dir, wire.PlanFile))
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("%s: %w", wire.PlanFile, err)
	}
	for _, w := range p.Workloads {
		if !slices.Contains(wire.Workloads, w) {
			return p, fmt.Errorf("plan: unknown workload %q", w)
		}
	}
	if p.Rounds < 1 || p.Jobs < 1 {
		return p, fmt.Errorf("plan: rounds (%d) and jobs (%d) must be positive", p.Rounds, p.Jobs)
	}
	return p, nil
}

// result is what a phase's body measured besides the runner's own clock.
type result struct {
	ns    int64 // duration the tool itself reported; 0 keeps the runner's
	ops   int64
	bytes int64
	extra map[string]float64
}

// phase is one timed step of a workload.
type phase struct {
	name   string       // one of wire.Phases
	before func() error // untimed preparation; may be nil
	cold   bool         // whether a cold reset runs after before
	body   func() (result, error)
	after  func() error // untimed teardown; may be nil
}

// phases runs ps in order as round's phases.
func (r *runner) phases(round int, ps []phase) error {
	for _, p := range ps {
		if err := r.measure(round, p); err != nil {
			return fmt.Errorf("%s: %w", p.name, err)
		}
	}
	return nil
}

// measure runs one phase: its untimed preparation and cold reset, the begin
// event, then only the body under a monotonic clock, the end event and the
// untimed teardown, with wire.Settle of idle after each event.
func (r *runner) measure(round int, p phase) error {
	if p.before != nil {
		if err := p.before(); err != nil {
			return err
		}
	}
	if p.cold {
		if err := r.coldReset(); err != nil {
			return fmt.Errorf("cold reset: %w", err)
		}
	}
	emit(wire.Event{Ev: wire.EvBegin, Phase: p.name, Round: round})
	time.Sleep(wire.Settle)
	start := time.Now()
	res, err := p.body()
	elapsed := time.Since(start)
	if err != nil {
		return err
	}
	if res.ns == 0 {
		res.ns = elapsed.Nanoseconds()
	}
	emit(wire.Event{
		Ev:    wire.EvEnd,
		Phase: p.name,
		Round: round,
		NS:    res.ns,
		Ops:   res.ops,
		Bytes: res.bytes,
		Extra: res.extra,
	})
	time.Sleep(wire.Settle)
	if p.after != nil {
		return p.after()
	}
	return nil
}

// emit writes ev as one console line; a single Write keeps the line whole
// when kernel messages share the console.
func emit(ev wire.Event) {
	data, err := json.Marshal(ev)
	if err != nil {
		// Only a non-finite Extra value fails: a bug guarded reports.
		panic(err)
	}
	line := make([]byte, 0, len(wire.Prefix)+len(data)+1)
	line = append(append(append(line, wire.Prefix...), data...), '\n')
	// A console that fails a write leaves nowhere to report it.
	os.Stdout.Write(line)
}

// powerOff flushes and detaches the tree and turns the VM off. PID 1 must
// not exit, so if the kernel refuses it reports why and idles forever.
func (r *runner) powerOff() {
	unix.Sync()
	if r.mounted {
		// The sync above already flushed the tree; a failed unmount loses
		// nothing the host reads.
		_ = r.unmount()
	}
	err := unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	emit(wire.Event{Ev: wire.EvFatal, Msg: fmt.Sprintf("power off: %v", err)})
	for {
		time.Sleep(time.Hour)
	}
}

// children is held while the runner waits for a command it started, so the
// reaper never collects an exit status os/exec is waiting for. Commands run
// one at a time, so any child exiting outside it is an orphan.
var children sync.Mutex

// reapOrphans collects every exited child on each SIGCHLD: as PID 1 the
// runner inherits the orphans of every command, and their zombies would
// otherwise accumulate over a run.
func reapOrphans() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, unix.SIGCHLD)
	go func() {
		for range sig {
			children.Lock()
			for {
				pid, err := unix.Wait4(-1, nil, unix.WNOHANG, nil)
				if err == unix.EINTR {
					continue
				}
				if err != nil || pid <= 0 {
					break
				}
			}
			children.Unlock()
		}
	}()
}

// command prepares name with args to run in dir.
func command(dir, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd
}

// runCmd runs cmd to completion and returns its output: stdout and stderr
// combined, or only stderr when the caller set cmd.Stdout. A missing binary
// or an exit status other than 0 and accepted is an error carrying the
// output's tail.
func runCmd(cmd *exec.Cmd, accepted ...int) ([]byte, error) {
	var out bytes.Buffer
	if cmd.Stdout == nil {
		cmd.Stdout = &out
	}
	cmd.Stderr = &out
	children.Lock()
	err := cmd.Run()
	children.Unlock()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && slices.Contains(accepted, exit.ExitCode()) {
		err = nil
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s: %w\n%s", strings.Join(cmd.Args, " "), err, tail(out.Bytes(), outputTail))
	}
	return out.Bytes(), nil
}

// tail returns b's last n bytes.
func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}
