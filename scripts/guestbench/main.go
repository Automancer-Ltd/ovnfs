//go:build darwin || linux

// Command guestbench times identical filesystem workloads inside vibevmm
// microVM guests on one fixture tree reached several ways, so a guest-native
// ovnfs client has measured targets to beat:
//
//	remotefs  the checkout shared over remote-fs v1, mounted as virtio-fs
//	nfs3      the checkout over the daemon's NFSv3 port, through user-net
//	nfs4      the same over NFSv4.0
//	ext4      the fixture unpacked on the guest's root disk: the ceiling
//	erofs     an EROFS image of the work tree behind the root filesystem,
//	          the read-only lower of an overlayfs (opt-in; built with
//	          mkfs.erofs in the -mkfs-image docker image)
//	kmod      the checkout through the ovnfs kernel module, which traps into
//	          vmon for every uncached operation; vmon relays to the daemon's
//	          kernel share (opt-in; -ko, or built with kmod/build.sh)
//
// One private daemon (temporary state, mount and HOME below /tmp, where Unix
// socket paths stay short) serves every checkout. Each config boots its own
// VM from a private clone of the -rootfs image; the guest runner
// (./guest, PID 1) reads its plan from a read-only virtio-fs share and
// reports every phase on the console (package wire), and the daemon's
// counters are sampled around each phase. Medians go to stdout as markdown;
// -out keeps everything raw.
//
// Example, from the module root:
//
//	go run ./scripts/guestbench -vmon ../vibevmm/target/release/vmon \
//		-kernel ../vibevmm/target/kernel/Image-aarch64 \
//		-rootfs ~/.cache/ovnfs-guestbench/rootfs-aarch64.ext4 -out gb.json
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/scripts/buildbench/fixture"
	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// Configs: the ways the guest reaches the tree, each one column.
const (
	cfgRemoteFS = "remotefs"
	cfgNFS3     = "nfs3"
	cfgNFS4     = "nfs4"
	cfgExt4     = "ext4"
	cfgErofs    = "erofs"
	cfgKmod     = "kmod"
)

// defaultConfigs lists the configs -configs selects by default, in column
// order.
var defaultConfigs = []string{cfgRemoteFS, cfgNFS3, cfgNFS4, cfgExt4}

// allConfigs lists every config -configs accepts.
var allConfigs = append(slices.Clone(defaultConfigs), cfgErofs, cfgKmod)

// needsCheckout reports whether cfg reaches the tree through a daemon
// checkout; the others keep it on the guest's disk.
func needsCheckout(cfg string) bool {
	return cfg == cfgRemoteFS || cfg == cfgNFS3 || cfg == cfgNFS4 || cfg == cfgKmod
}

// guestArch names the host's architecture as Linux and the rootfs image tags
// do (aarch64, x86_64); the guest runs the host's architecture.
func guestArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	}
	return runtime.GOARCH
}

// options holds the command line; -out records it as the run's flags.
type options struct {
	VMON      string        `json:"vmon"`
	Kernel    string        `json:"kernel"`
	Rootfs    string        `json:"rootfs"`
	OVNFS     string        `json:"ovnfs,omitempty"` // empty: built from the working tree
	Configs   []string      `json:"configs"`
	Workloads []string      `json:"workloads"`
	Runs      int           `json:"runs"`
	Cold      int           `json:"cold"`
	Files     int           `json:"files"`
	Project   string        `json:"project,omitempty"`
	CPUs      int           `json:"cpus"`
	Mem       int           `json:"mem_mib"`
	Jobs      int           `json:"jobs"`
	Stall     time.Duration `json:"stall_ns"`
	Out       string        `json:"out,omitempty"`
	Keep      bool          `json:"keep"`
	VMONArgs  []string      `json:"vmon_args,omitempty"`
	MkfsImage string        `json:"mkfs_image"`
	KO        string        `json:"ko,omitempty"` // empty: built with kmod/build.sh
}

func main() {
	o, err := parseFlags()
	if err != nil {
		fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Unix socket paths are short, so the state lives below /tmp, not $TMPDIR.
	root, err := os.MkdirTemp("/tmp", "gb-")
	if err != nil {
		fail(err)
	}
	err = run(ctx, o, root)
	if o.Keep {
		fmt.Fprintln(os.Stderr, "guestbench: kept", root)
	} else if rmErr := os.RemoveAll(root); rmErr != nil {
		fmt.Fprintln(os.Stderr, "guestbench:", rmErr)
	}
	if err != nil {
		fail(err)
	}
}

// fail reports err and exits non-zero.
func fail(err error) {
	fmt.Fprintln(os.Stderr, "guestbench:", err)
	os.Exit(1)
}

// parseFlags reads and validates the command line.
func parseFlags() (*options, error) {
	o := &options{}
	flag.StringVar(&o.VMON, "vmon", "", "vibevmm's vmon binary (required)")
	flag.StringVar(&o.Kernel, "kernel", "", "guest kernel image (required)")
	flag.StringVar(&o.Rootfs, "rootfs", "", "base ext4 rootfs image, never modified (required)")
	flag.StringVar(&o.OVNFS, "ovnfs", "", "ovnfs binary to serve the checkouts (default: built from the working tree)")
	configs := flag.String("configs", strings.Join(defaultConfigs, ","), "comma-separated configs: "+strings.Join(allConfigs, ", "))
	workloads := flag.String("workloads", strings.Join(wire.Workloads, ","), "comma-separated workloads: "+strings.Join(wire.Workloads, ", "))
	flag.IntVar(&o.Runs, "runs", 3, "rounds of every workload; phases report the median")
	flag.IntVar(&o.Cold, "cold", 1, "leading rounds that run cargo.cold")
	flag.IntVar(&o.Files, "files", 50000, "bulk files in the fixture besides the crate")
	flag.StringVar(&o.Project, "project", "", "cargo project whose files form the fixture crate (default: a synthetic crate)")
	flag.IntVar(&o.CPUs, "cpus", 4, "guest vCPUs")
	flag.IntVar(&o.Mem, "mem", 8192, "guest memory in MiB")
	flag.IntVar(&o.Jobs, "jobs", 4, "parallelism of the small and replay workloads")
	flag.DurationVar(&o.Stall, "stall", 10*time.Minute, "time without a guest event after which the VM is killed")
	flag.StringVar(&o.Out, "out", "", "write the raw results as JSON to this file")
	flag.BoolVar(&o.Keep, "keep", false, "keep the temporary directory and print its path")
	flag.StringVar(&o.MkfsImage, "mkfs-image", "ovnfs-guestbench-rootfs:"+guestArch(), "docker image with mkfs.erofs, which builds the erofs config's image (scripts/guestbench/rootfs/build.sh)")
	flag.StringVar(&o.KO, "ko", "", "prebuilt ovnfs kernel module for the kmod config (default: built with kmod/build.sh)")
	flag.Func("vmon-arg", "argument appended to the vmon command line (repeatable)", func(s string) error {
		o.VMONArgs = append(o.VMONArgs, s)
		return nil
	})
	flag.Parse()
	if flag.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments %q", flag.Args())
	}
	for _, p := range []struct {
		name string
		path *string
	}{{"vmon", &o.VMON}, {"kernel", &o.Kernel}, {"rootfs", &o.Rootfs}, {"ovnfs", &o.OVNFS}, {"ko", &o.KO}} {
		if *p.path == "" {
			if p.name == "ovnfs" || p.name == "ko" {
				continue
			}
			return nil, fmt.Errorf("-%s is required", p.name)
		}
		abs, err := filepath.Abs(*p.path)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("-%s: %w", p.name, err)
		}
		*p.path = abs
	}
	if o.Project != "" {
		abs, err := filepath.Abs(o.Project)
		if err != nil {
			return nil, err
		}
		o.Project = abs
	}
	var err error
	if o.Configs, err = pick("config", *configs, allConfigs); err != nil {
		return nil, err
	}
	if o.Workloads, err = pick("workload", *workloads, wire.Workloads); err != nil {
		return nil, err
	}
	// The runner runs workloads in wire order whatever the plan lists.
	slices.SortFunc(o.Workloads, func(a, b string) int {
		return slices.Index(wire.Workloads, a) - slices.Index(wire.Workloads, b)
	})
	switch {
	case o.Runs < 1:
		return nil, errors.New("-runs must be at least 1")
	case o.Cold < 0 || o.Cold > o.Runs:
		return nil, errors.New("-cold must be between 0 and -runs")
	case o.Files < 0:
		return nil, errors.New("-files must not be negative")
	case o.CPUs < 1 || o.Mem < 256 || o.Jobs < 1:
		return nil, errors.New("-cpus and -jobs must be at least 1, -mem at least 256")
	case o.Stall <= 0:
		return nil, errors.New("-stall must be positive")
	}
	return o, nil
}

// pick splits a comma-separated list of names, each one of known, refusing
// duplicates; the order given is kept.
func pick(what, list string, known []string) ([]string, error) {
	var names []string
	for name := range strings.SplitSeq(list, ",") {
		name = strings.TrimSpace(name)
		switch {
		case !slices.Contains(known, name):
			return nil, fmt.Errorf("unknown %s %q (want %s)", what, name, strings.Join(known, ", "))
		case slices.Contains(names, name):
			return nil, fmt.Errorf("%s %q given twice", what, name)
		}
		names = append(names, name)
	}
	return names, nil
}

// bench is one invocation's settings and the resources its configs share.
type bench struct {
	o        *options
	root     string // the temporary directory
	share    string // the guest's bench share
	repo     *fixture.Repo
	daemon   *daemon // nil when no config needs a checkout
	echoPort int
	erofs    *erofsImage   // nil unless the erofs config runs
	module   *kernelModule // nil unless the kmod config runs
}

// run builds the fixture, the daemon and the runner, measures every config,
// and reports. An interrupted run still reports the configs it finished.
func run(ctx context.Context, o *options, root string) error {
	mod, err := moduleRoot(ctx)
	if err != nil {
		return err
	}
	b := &bench{o: o, root: root, share: filepath.Join(root, "share")}
	if err := os.Mkdir(b.share, 0o755); err != nil {
		return err
	}
	start := time.Now()
	if b.repo, err = fixture.Make(filepath.Join(root, "fixture"), o.Project, o.Files); err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	fmt.Fprintf(os.Stderr, "guestbench: fixture: crate %s + %d bulk files in %.1fs\n", cmp.Or(o.Project, "synthetic"), o.Files, time.Since(start).Seconds())
	res := newResults(o, b.repo)
	if slices.ContainsFunc(o.Configs, needsCheckout) {
		bin := o.OVNFS
		if bin == "" {
			bin = filepath.Join(root, "ovnfs")
			if err := goBuild(ctx, mod, bin, ".", nil); err != nil {
				return err
			}
		}
		if b.daemon, err = startDaemon(ctx, bin, root); err != nil {
			return err
		}
		defer b.daemon.stop()
		res.addBinary("ovnfs", bin, b.daemon.version)
	}
	guest := filepath.Join(b.share, wire.Runner)
	linux := []string{"GOOS=linux", "GOARCH=" + runtime.GOARCH, "CGO_ENABLED=0"}
	if err := goBuild(ctx, mod, guest, "./scripts/guestbench/guest", linux); err != nil {
		return err
	}
	res.addBinary("guest", guest, "")
	echo, err := startEcho()
	if err != nil {
		return err
	}
	defer echo.Close()
	b.echoPort = echo.port()
	// A checkout's .git is a file naming a host path, so every config gets
	// the git directory on the guest's disk.
	if err := writeTar(filepath.Join(b.repo.Dir, ".git"), filepath.Join(b.share, wire.GitTar), ""); err != nil {
		return fmt.Errorf("git tar: %w", err)
	}
	if slices.Contains(o.Configs, cfgExt4) {
		if err := writeTar(b.repo.Dir, filepath.Join(b.share, wire.FixtureTar), ".git"); err != nil {
			return fmt.Errorf("fixture tar: %w", err)
		}
	}
	if slices.Contains(o.Configs, cfgErofs) {
		b.erofs = buildErofs(ctx, o.MkfsImage, b.repo.Dir, root)
		res.Erofs = b.erofs
	}
	if slices.Contains(o.Configs, cfgKmod) {
		b.module = prepareModule(ctx, o.KO, mod, root, b.share)
		res.Module = b.module
	}
	for _, cfg := range o.Configs {
		if ctx.Err() != nil {
			break
		}
		r := b.runConfig(ctx, cfg)
		res.Configs = append(res.Configs, r)
		if r.Error != "" {
			fmt.Fprintf(os.Stderr, "guestbench: %s failed: %s\n", cfg, r.Error)
		}
	}
	res.print(os.Stdout)
	if o.Out != "" {
		if err := res.write(o.Out); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	return nil
}

// runConfig measures one config in its own VM, recording a failure in the
// result rather than returning it, so the other configs still run.
func (b *bench) runConfig(ctx context.Context, cfg string) *configResult {
	r := &configResult{Config: cfg}
	start := time.Now()
	if err := b.measure(ctx, r); err != nil {
		r.Error = err.Error()
	}
	r.Seconds = time.Since(start).Seconds()
	return r
}

// measure opens the config's checkout (none for ext4 and erofs), boots its
// VM and follows the runner until it is done, then removes the checkout.
func (b *bench) measure(ctx context.Context, r *configResult) error {
	if r.Config == cfgErofs && b.erofs.err != nil {
		return b.erofs.err
	}
	if r.Config == cfgKmod && b.module.err != nil {
		return b.module.err
	}
	sock := ""
	if needsCheckout(r.Config) {
		co, err := b.daemon.newCheckout(ctx, b.repo.Dir, "gb-"+r.Config)
		if err != nil {
			return err
		}
		r.Checkout = co.Name
		defer func() {
			// Removal runs after an interrupt too, so it gets its own deadline.
			rmCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := b.daemon.remove(rmCtx, co.Name); err != nil {
				fmt.Fprintln(os.Stderr, "guestbench:", err)
			}
		}()
		if r.Config == cfgRemoteFS || r.Config == cfgKmod {
			share := api.ShareArgs{Checkout: co.Name, Writable: true, Kernel: r.Config == cfgKmod}
			if sock, err = b.daemon.share(ctx, share); err != nil {
				return err
			}
		}
	}
	d, err := privateDisk(b.o.Rootfs, b.root, r.Config)
	if err != nil {
		return err
	}
	defer d.release()
	r.Disk = d.kind
	var image *wire.Image
	if r.Config == cfgErofs {
		if image, err = d.appendImage(b.erofs.path); err != nil {
			return err
		}
	}
	r.Plan = b.plan(r.Config, r.Checkout, image)
	if err := writePlan(b.share, r.Plan); err != nil {
		return err
	}
	v, err := startVM(b.o.VMON, b.vmonArgs(d, r.Config, sock), filepath.Join(b.root, r.Config+".console.log"))
	if err != nil {
		return err
	}
	defer v.terminate(0)
	return b.follow(ctx, v, r)
}

// beginning is a begin event and the daemon's counters when it arrived.
type beginning struct {
	ev     wire.Event
	sample *sample
}

// follow consumes the runner's events until done, sampling the daemon's
// counters at every begin and end so each phase gets its own delta. A fatal
// event, vmon exiting early or -stall without an event fails the config with
// the console's last lines.
func (b *bench) follow(ctx context.Context, v *vm, r *configResult) error {
	sampled := b.daemon != nil && r.Checkout != ""
	stall := time.NewTimer(b.o.Stall)
	defer stall.Stop()
	begun := map[string]beginning{}
	key := func(ev wire.Event) string { return fmt.Sprintf("%s/%d", ev.Phase, ev.Round) }
	for {
		select {
		case <-ctx.Done():
			return v.failure(errors.New("interrupted"))
		case <-stall.C:
			return v.failure(fmt.Errorf("no guest event for %s", b.o.Stall))
		case ev, ok := <-v.events:
			if !ok {
				v.terminate(5 * time.Second)
				return v.failure(fmt.Errorf("vmon exited before the runner was done: %v", v.err))
			}
			stall.Reset(b.o.Stall)
			r.Events = append(r.Events, ev)
			switch ev.Ev {
			case wire.EvBoot:
				fmt.Fprintf(os.Stderr, "guestbench: %s: runner up\n", r.Config)
			case wire.EvBegin:
				bg := beginning{ev: ev}
				if sampled {
					bg.sample = b.sampleDaemon(ctx)
				}
				begun[key(ev)] = bg
			case wire.EvEnd:
				pr := phaseRun{Phase: ev.Phase, Round: ev.Round, End: ev}
				if bg, ok := begun[key(ev)]; ok {
					pr.Begin = &bg.ev
					if bg.sample != nil {
						if s := b.sampleDaemon(ctx); s != nil {
							pr.Daemon = s.since(bg.sample)
						}
					}
				}
				r.Runs = append(r.Runs, pr)
				fmt.Fprintf(os.Stderr, "guestbench: %s: %s round %d: %s\n", r.Config, ev.Phase, ev.Round, formatSeconds(float64(ev.NS)/1e9))
			case wire.EvFatal:
				return v.failure(fmt.Errorf("guest: %s", ev.Msg))
			case wire.EvDone:
				// The runner powers the VM off next; vmon then exits 0.
				v.terminate(10 * time.Second)
				return nil
			}
		}
	}
}

// sampleDaemon samples the daemon's counters, reporting a failure on stderr
// and returning nil: a missing delta leaves blanks, not a failed config.
func (b *bench) sampleDaemon(ctx context.Context) *sample {
	s, err := b.daemon.sample(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "guestbench: sampling daemon:", err)
		return nil
	}
	return s
}

// moduleRoot finds the ovnfs module's root, where the daemon and the runner
// are built from.
func moduleRoot(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == os.DevNull {
		return "", errors.New("run guestbench from inside the ovnfs module")
	}
	return filepath.Dir(mod), nil
}

// goBuild builds pkg of the module at dir into out, with env added to the
// environment (a cross build's GOOS and GOARCH).
func goBuild(ctx context.Context, dir, out, pkg string, env []string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if msg, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %w\n%s", pkg, err, msg)
	}
	return nil
}
