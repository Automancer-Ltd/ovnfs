//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/stencil-hq/ovnfs/scripts/guestbench/wire"
)

// sysNet lists the guest's network interfaces.
const sysNet = "/sys/class/net"

// configureNet brings up loopback and the VM's NIC with n's static address
// and default route.
func configureNet(n *wire.Network) error {
	nic, err := nicName()
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"link", "set", "lo", "up"},
		{"link", "set", nic, "up"},
		{"addr", "add", n.Addr, "dev", nic},
		{"route", "add", "default", "via", n.Gateway, "dev", nic},
	} {
		if _, err := runCmd(command("/", "ip", args...)); err != nil {
			return err
		}
	}
	return nil
}

// nicName returns the first interface backed by a device: the virtio-net
// NIC, where the kernel's built-in dummy and tunnel interfaces have none.
func nicName() (string, error) {
	entries, err := os.ReadDir(sysNet)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if _, err := os.Lstat(filepath.Join(sysNet, e.Name(), "device")); err == nil {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("no device-backed interface in %s", sysNet)
}

// fsTypes maps the kinds mounted from a source to their mount(8) filesystem
// types.
var fsTypes = map[wire.Kind]string{
	wire.KindVirtiofs: "virtiofs",
	wire.KindNFS:      "nfs",
	wire.KindModule:   moduleFS,
}

// moduleFS is the filesystem type, and the mount source, the ovnfs kernel
// module registers.
const moduleFS = "ovnfs"

// prepareTree makes the plan's tree reachable at r.root beside the fixture's
// git directory at wire.GitDir, hides the workloads' scratch directories
// from git and brings the index's stat data up to date, so no timed git
// status pays for a refresh. An ovnfs checkout's .git is a file naming a
// host path, so git never uses the tree's own.
func (r *runner) prepareTree() error {
	if err := unpack(wire.GitTar, wire.GitDir); err != nil {
		return err
	}
	base := wire.Mount
	switch r.plan.Kind {
	case wire.KindLocal:
		base = wire.Local
		if err := unpack(wire.FixtureTar, base); err != nil {
			return err
		}
	case wire.KindVirtiofs, wire.KindNFS:
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
		if err := r.mount(); err != nil {
			return err
		}
	case wire.KindModule:
		if _, err := runCmd(command("/", "insmod", filepath.Join(wire.Dir, wire.ModuleFile))); err != nil {
			return err
		}
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
		if err := r.mount(); err != nil {
			return err
		}
	case wire.KindImage:
		lower, err := mapImage(r.plan.Image)
		if err != nil {
			return err
		}
		r.lower = lower
		for _, dir := range []string{base, wire.Lower, overlayUpper, overlayWork} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		if err := r.mount(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown kind %q", r.plan.Kind)
	}
	r.root = filepath.Join(base, r.plan.Subdir)
	if err := excludeScratch(); err != nil {
		return err
	}
	_, err := runCmd(r.gitCmd("status", "--porcelain"))
	return err
}

// unpack extracts the share's archive into dir, creating dir.
func unpack(archive, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	_, err := runCmd(command("/", "tar", "-xpf", filepath.Join(wire.Dir, archive), "-C", dir))
	return err
}

// gitCmd prepares git with args on the tree and wire.GitDir; fsmonitor
// stays off so every status compares the work tree itself.
func (r *runner) gitCmd(args ...string) *exec.Cmd {
	return command(r.root, "git", append([]string{
		"--git-dir=" + wire.GitDir,
		"--work-tree=" + r.root,
		"-c", "core.fsmonitor=false",
	}, args...)...)
}

// scratchDirs are the directories the workloads create in the tree, which
// git must not report or walk as untracked.
var scratchDirs = []string{"target/", "scratch/"}

// excludeScratch appends whichever of scratchDirs the git directory's
// info/exclude lacks, so a git directory reused across boots gains them
// once.
func excludeScratch() error {
	info := filepath.Join(wire.GitDir, "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	path := filepath.Join(info, "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	lines := strings.Split(string(data), "\n")
	var missing []string
	for _, d := range scratchDirs {
		if !slices.Contains(lines, d) {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	text := strings.Join(missing, "\n") + "\n"
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		text = "\n" + text
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// The overlay's directories on the root filesystem; the upper one keeps the
// tree's changes across cold resets, as a server does for the mounted kinds.
const (
	overlayUpper = wire.Overlay + "/upper"
	overlayWork  = wire.Overlay + "/work"
)

// overlayOptions stacks the overlay's upper directory on the mounted image;
// redirect_dir lets directories of the image be renamed.
const overlayOptions = "lowerdir=" + wire.Lower + ",upperdir=" + overlayUpper +
	",workdir=" + overlayWork + ",redirect_dir=on"

// mapImage attaches img's byte range to a free read-only loop device and
// returns its node. A loop device, unlike a device-mapper table, may share
// the disk the root filesystem holds exclusively; it stays attached for the
// VM's life.
func mapImage(img *wire.Image) (string, error) {
	if img == nil {
		return "", errors.New("plan has no image")
	}
	if img.Offset < 0 || img.Size <= 0 || img.Offset%512 != 0 || img.Size%512 != 0 {
		return "", fmt.Errorf("image range %d+%d is not sector-aligned", img.Offset, img.Size)
	}
	// Only stdout names the device; warnings go to stderr.
	var out bytes.Buffer
	cmd := command("/", "losetup", "--find", "--show", "--read-only", "--direct-io=on",
		"--offset", fmt.Sprint(img.Offset), "--sizelimit", fmt.Sprint(img.Size), img.Device)
	cmd.Stdout = &out
	if _, err := runCmd(cmd); err != nil {
		return "", err
	}
	node := strings.TrimSpace(out.String())
	if node == "" {
		return "", fmt.Errorf("losetup named no loop device for %s", img.Device)
	}
	return node, nil
}

// mount mounts the tree on wire.Mount: the plan's source through mount(8),
// whose mount.nfs helper resolves the NFS options (KindModule's source is
// moduleFS), or for KindImage the image on wire.Lower beneath an overlay.
func (r *runner) mount() error {
	var mounts [][]string
	if r.plan.Kind == wire.KindImage {
		mounts = [][]string{
			{"-t", "erofs", "-o", "ro", r.lower, wire.Lower},
			{"-t", "overlay", "-o", overlayOptions, "overlay", wire.Mount},
		}
	} else {
		args := []string{"-t", fsTypes[r.plan.Kind]}
		if r.plan.Options != "" {
			args = append(args, "-o", r.plan.Options)
		}
		source := r.plan.Source
		if r.plan.Kind == wire.KindModule {
			source = moduleFS
		}
		mounts = [][]string{append(args, source, wire.Mount)}
	}
	for _, args := range mounts {
		if _, err := runCmd(command("/", "mount", args...)); err != nil {
			return err
		}
	}
	r.mounted = true
	return nil
}

// unmount detaches the tree from wire.Mount, and for KindImage then the
// image from wire.Lower.
func (r *runner) unmount() error {
	targets := []string{wire.Mount}
	if r.plan.Kind == wire.KindImage {
		targets = append(targets, wire.Lower)
	}
	for _, t := range targets {
		if err := unix.Unmount(t, 0); err != nil {
			return fmt.Errorf("unmount %s: %w", t, err)
		}
	}
	r.mounted = false
	return nil
}

// coldReset makes the next phase start cold: dirty data written back, the
// tree remounted so its filesystems forget every inode and attribute
// (KindLocal has nothing mounted to reset), and the guest's page, dentry and
// inode caches dropped.
func (r *runner) coldReset() error {
	unix.Sync()
	if r.plan.Kind != wire.KindLocal {
		if err := r.unmount(); err != nil {
			return err
		}
		if err := r.mount(); err != nil {
			return err
		}
	}
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0o644)
}

// kernelLogLines is how many trailing kernel log lines a KindModule failure
// report carries.
const kernelLogLines = 20

// kernelLog returns the kernel log's last lines, where a failed module load
// or a filesystem bug in the module explains itself.
func kernelLog() string {
	out, err := dmesg()
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	return strings.Join(lines[max(0, len(lines)-kernelLogLines):], "\n")
}

// dmesg returns the kernel log as util-linux dmesg prints it with args.
func dmesg(args ...string) ([]byte, error) {
	return runCmd(command("/", "dmesg", args...))
}

// warningBytes bounds the kernel warnings a KindModule run reports.
const warningBytes = 4 << 10

// warningMarkers pick the lines of an unfiltered kernel log that tell of a
// kernel or module problem, for a dmesg that cannot filter by level.
var warningMarkers = []string{"WARNING", "BUG", "Oops", "Call trace", "ovnfs", "rcu", "lockdep"}

// kernelWarnings returns the kernel log's warnings and worse, at most the
// last warningBytes of them in whole lines, or "" when there are none:
// the kernel boots quiet, so a module's WARN splats reach the host only
// this way.
func kernelWarnings() string {
	out, err := dmesg("--level=emerg,alert,crit,err,warn")
	if err != nil {
		all, err := dmesg()
		if err != nil {
			return err.Error()
		}
		out = nil
		for line := range bytes.Lines(all) {
			if slices.ContainsFunc(warningMarkers, func(m string) bool { return bytes.Contains(line, []byte(m)) }) {
				out = append(out, line...)
			}
		}
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return ""
	}
	if len(out) > warningBytes {
		out = out[len(out)-warningBytes:]
		// Drop the line the cut split.
		if i := bytes.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
	}
	return string(out)
}

// debugfs is where the kernel's debug filesystem is mounted.
const debugfs = "/sys/kernel/debug"

// moduleCalls returns the ovnfs module's call statistics from debugfs,
// mounting debugfs first when needed, or "" when they cannot be read: they
// annotate the run and never fail it.
func moduleCalls() string {
	path := filepath.Join(debugfs, "ovnfs", "calls")
	data, err := os.ReadFile(path)
	if err != nil {
		// Already mounted or impossible; the second read tells which.
		_ = unix.Mount("debugfs", debugfs, "debugfs", 0, "")
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return ""
	}
	return string(data)
}

// syncTree writes back everything pending on the tree's filesystem with
// syncfs(2) and waits for it, without flushing the guest's other
// filesystems as sync would.
func (r *runner) syncTree() error {
	f, err := os.Open(r.root)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Syncfs(int(f.Fd())); err != nil {
		return fmt.Errorf("syncfs %s: %w", r.root, err)
	}
	return nil
}

// target is the tree's cargo target directory.
func (r *runner) target() string { return filepath.Join(r.root, "target") }

// scratch is the small workload's directory in the tree.
func (r *runner) scratch() string { return filepath.Join(r.root, "scratch") }

// removeTarget deletes the tree's target directory, so a cold build starts
// from nothing.
func (r *runner) removeTarget() error { return os.RemoveAll(r.target()) }
