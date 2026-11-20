//go:build darwin || linux

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/admin"
	"github.com/stencil-hq/ovnfs/api"
)

// maxSocket bounds a Unix socket path: sun_path holds 104 bytes on macOS
// (108 on Linux), terminating NUL included.
const maxSocket = 104

// daemonStart bounds how long a fresh daemon may take to answer its hello:
// it answers once it serves NFS on its port and has mounted it.
const daemonStart = 2 * time.Minute

// daemon is the private ovnfs daemon serving every checkout: its own state,
// mount and HOME below the run's directory, so the user's daemon and
// settings are never touched.
type daemon struct {
	state   string
	mount   string
	log     string // the daemon's stdout and stderr
	cmd     *exec.Cmd
	exited  chan struct{} // closed once cmd has been waited for
	client  *admin.Client
	port    int    // the NFS port on 127.0.0.1, MOUNT on the same one
	version string // the daemon's build, from status
	started time.Time
}

// startDaemon runs `bin serve` serving NFS on a free TCP port of 127.0.0.1,
// which the guest reaches through user-net's gateway, and connects to its
// admin socket. The client's auto-start cannot pick the protocol or listen
// address, so serve runs as a child here, in its own process group so a
// terminal's interrupt reaches only guestbench, which then stops it in order.
func startDaemon(ctx context.Context, bin, root string) (*daemon, error) {
	d := &daemon{state: filepath.Join(root, "state"), mount: filepath.Join(root, "mnt"), log: filepath.Join(root, "serve.log"), exited: make(chan struct{}), started: time.Now()}
	home := filepath.Join(root, "home")
	for _, dir := range []string{d.state, d.mount, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	sock := filepath.Join(d.state, "admin.sock")
	if err := checkSocket(sock); err != nil {
		return nil, err
	}
	logFile, err := os.Create(d.log)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	d.cmd = exec.Command(bin, "serve", "--proto", "nfs", "--listen", "127.0.0.1:0")
	d.cmd.Env = append(os.Environ(), "OVNFS_STATE="+d.state, "OVNFS_MOUNT="+d.mount, "HOME="+home, "OVNFS_GIT_REMOTES=false")
	d.cmd.Dir = d.state
	d.cmd.Stdout, d.cmd.Stderr = logFile, logFile
	d.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := d.cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting ovnfs serve: %w", err)
	}
	go func() {
		d.cmd.Wait()
		close(d.exited)
	}()
	if err := d.connect(ctx, sock); err != nil {
		d.stop()
		return nil, err
	}
	return d, nil
}

// connect dials the admin socket until the daemon answers its hello, then
// learns its NFS port and build.
func (d *daemon) connect(ctx context.Context, sock string) error {
	c, err := d.dial(ctx, sock)
	if err != nil {
		return err
	}
	d.client = c
	w := c.Welcome()
	if w.Proto != "nfs" || w.Port == 0 {
		return fmt.Errorf("ovnfs serves %s on port %d, want NFS over TCP", w.Proto, w.Port)
	}
	d.port = w.Port
	var st api.Status
	if err := d.call(ctx, "status", struct{}{}, &st); err != nil {
		return err
	}
	d.version = st.BuildVersion
	return nil
}

// dial retries the admin socket until the daemon answers, exits, ctx ends
// or daemonStart passes. A daemon holds the hello until it serves, so a dial
// that connects waits that out.
func (d *daemon) dial(ctx context.Context, sock string) (*admin.Client, error) {
	type dialed struct {
		c   *admin.Client
		err error
	}
	deadline := time.Now().Add(daemonStart)
	for {
		done := make(chan dialed, 1)
		go func() {
			c, err := admin.Dial(sock, "guestbench")
			done <- dialed{c, err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-d.exited:
			return nil, fmt.Errorf("ovnfs serve exited during start:\n%s", d.logTail())
		case r := <-done:
			if r.err == nil {
				return r.c, nil
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("ovnfs serve did not answer on %s within %s: %w", sock, daemonStart, r.err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// call runs one admin operation, decoding its reply into reply unless nil.
func (d *daemon) call(ctx context.Context, op string, args, reply any) error {
	raw, err := d.client.Call(ctx, op, args)
	if err != nil {
		return fmt.Errorf("ovnfs %s: %w", op, err)
	}
	if reply == nil {
		return nil
	}
	if err := json.Unmarshal(raw, reply); err != nil {
		return fmt.Errorf("ovnfs %s: decoding reply: %w", op, err)
	}
	return nil
}

// newCheckout opens a checkout name of dir.
func (d *daemon) newCheckout(ctx context.Context, dir, name string) (api.Checkout, error) {
	var r api.NewReply
	err := d.call(ctx, "new", api.NewArgs{Dir: dir, Name: name}, &r)
	return r.Checkout, err
}

// share serves the checkout a names over remote-fs, or to vmon's kernel
// client when a.Kernel is set, and returns the socket, checked for length.
func (d *daemon) share(ctx context.Context, a api.ShareArgs) (string, error) {
	var r api.ShareReply
	if err := d.call(ctx, "share", a, &r); err != nil {
		return "", err
	}
	return r.Socket, checkSocket(r.Socket)
}

// remove removes the checkout and its snapshots.
func (d *daemon) remove(ctx context.Context, name string) error {
	return d.call(ctx, "rm", api.RmArgs{Checkout: name}, nil)
}

// stop asks the daemon to unmount and exit, asking twice since a second stop
// forces a busy unmount (api.StopArgs); a daemon that does not exit gets
// SIGTERM, which unmounts too, then SIGKILL and a forced unmount. Every exit
// path of a run calls it before the run's directory is removed.
func (d *daemon) stop() {
	if d.client != nil {
		for range 2 {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := d.call(ctx, "stop", api.StopArgs{}, nil)
			cancel()
			// Any error but the daemon's refusal is it closing the connection as it exits.
			if _, refused := errors.AsType[*admin.RemoteError](err); !refused {
				break
			}
			fmt.Fprintln(os.Stderr, "guestbench:", err)
		}
		d.client.Close()
		if d.awaitExit(time.Minute) {
			return
		}
	}
	syscall.Kill(-d.cmd.Process.Pid, syscall.SIGTERM)
	if d.awaitExit(15 * time.Second) {
		return
	}
	syscall.Kill(-d.cmd.Process.Pid, syscall.SIGKILL)
	<-d.exited
	if out, err := exec.Command("umount", "-f", d.mount).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "guestbench: umount -f %s: %v %s\n", d.mount, err, out)
	}
}

// awaitExit reports whether the daemon exits within wait.
func (d *daemon) awaitExit(wait time.Duration) bool {
	select {
	case <-d.exited:
		return true
	case <-time.After(wait):
		return false
	}
}

// logTail returns the end of the daemon's log, which the run's directory
// takes with it unless -keep.
func (d *daemon) logTail() string {
	out, err := os.ReadFile(d.log)
	if err != nil {
		return err.Error()
	}
	return string(out[max(0, len(out)-4096):])
}

// checkSocket refuses a Unix socket path too long to bind or connect to.
func checkSocket(path string) error {
	if len(path) >= maxSocket {
		return fmt.Errorf("socket path %s is %d bytes, over the limit of %d", path, len(path), maxSocket-1)
	}
	return nil
}

// timerKey names one daemon timer (api.StatsTimer).
type timerKey struct{ group, name string }

// sample is the daemon's cumulative counters at one instant.
type sample struct {
	cpu    float64 // user and system seconds
	timers map[timerKey]api.StatsTimer
}

// sample reads the daemon's counters over its whole life: the stats record
// is the daemon's own (a private state), so its totals are cumulative.
func (d *daemon) sample(ctx context.Context) (*sample, error) {
	var r api.StatsReply
	since := time.Since(d.started) + time.Hour
	if err := d.call(ctx, "stats", api.StatsArgs{Since: since.Seconds()}, &r); err != nil {
		return nil, err
	}
	s := &sample{cpu: r.CPU.User + r.CPU.Sys, timers: make(map[timerKey]api.StatsTimer, len(r.Timers))}
	for _, t := range r.Timers {
		s.timers[timerKey{t.Group, t.Name}] = t
	}
	return s, nil
}

// daemonDelta is what the daemon did between two samples.
type daemonDelta struct {
	CPU    float64      `json:"cpu_s"`
	Timers []timerDelta `json:"timers"`
}

// timerDelta is one timer's growth between two samples.
type timerDelta struct {
	Group string `json:"group"`
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Us    int64  `json:"us"`
}

// since returns what grew from prev to s, timers by group and name.
func (s *sample) since(prev *sample) *daemonDelta {
	d := &daemonDelta{CPU: s.cpu - prev.cpu, Timers: []timerDelta{}}
	for k, t := range s.timers {
		p := prev.timers[k]
		td := timerDelta{Group: k.group, Name: k.name, Count: t.Count - p.Count, Us: int64(math.Round((t.Seconds - p.Seconds) * 1e6))}
		if td.Count != 0 || td.Us != 0 {
			d.Timers = append(d.Timers, td)
		}
	}
	slices.SortFunc(d.Timers, func(a, b timerDelta) int {
		return cmp.Or(cmp.Compare(a.Group, b.Group), cmp.Compare(a.Name, b.Name))
	})
	return d
}

// timer returns the delta of one timer, zero when it did not grow.
func (d *daemonDelta) timer(group, name string) timerDelta {
	for _, t := range d.Timers {
		if t.Group == group && t.Name == name {
			return t
		}
	}
	return timerDelta{Group: group, Name: name}
}
