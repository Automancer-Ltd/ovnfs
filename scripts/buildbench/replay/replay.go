// Package replay reproduces the filesystem traffic of a cargo build without
// running any compiler: a cold build, a no-op build, a relink and a clean,
// with a fixed shape and byte-for-byte reproducible content, so the same work
// can be timed on any filesystem. It only uses the portable os package.
package replay

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// BinName is the name of the replayed binary crate; fixtures name their
// synthetic crate after it so a replayed build matches a real one.
const BinName = "app"

// unit is one library crate of the replayed build.
type unit struct {
	name, hash, scriptHash, runHash string
	rlib, rmeta                     int
	script                          bool
	level                           int
	deps                            []int // earlier units whose rmeta rustc reads
}

// Shape of the replayed build, measured on a cold `cargo build` of a
// 149-package workspace: 82 rlibs (median 130 KB, p90 1.7 MB, max 7 MB), 29
// build scripts, 126 fingerprint directories, 256 codegen objects of the
// binary and a 24 MB linked executable.
const (
	units       = 82
	levels      = 8
	scriptEvery = 3
	binObjects  = 256
	binObject   = 160 << 10
	binSize     = 24 << 20
	binHash     = "0f3c9a1b2d4e5f60"
)

// Build is a replayed cargo build of one crate: its shape, content and the
// crate sources it reads.
type Build struct {
	jobs    int
	sources []string // crate-relative source files rustc reads
	touch   string   // crate-relative source touched before relinking
	plan    []unit
	pattern []byte // compressible bytes written as artifact content
}

// New fixes the replayed build's shape and content for a crate whose sources
// (crate-relative) rustc reads and whose touch source is modified before a
// relink; jobs bounds the units built in parallel, like cargo -j.
func New(jobs int, sources []string, touch string) *Build {
	b := &Build{jobs: jobs, sources: sources, touch: touch}
	r := rand.New(rand.NewPCG(1, 2))
	for i := range units {
		u := unit{
			name:       fmt.Sprintf("dep%03d", i),
			hash:       fmt.Sprintf("%016x", r.Uint64()),
			scriptHash: fmt.Sprintf("%016x", r.Uint64()),
			runHash:    fmt.Sprintf("%016x", r.Uint64()),
			script:     i%scriptEvery == 0,
			level:      i * levels / units,
		}
		size := math.Exp(math.Log(130e3) + 2.0*r.NormFloat64())
		u.rlib = int(min(max(size, 8<<10), 7<<20))
		u.rmeta = u.rlib / 3
		for range min(6, i) {
			if d := r.IntN(i); b.plan == nil || b.plan[d].level < u.level {
				u.deps = append(u.deps, d)
			}
		}
		b.plan = append(b.plan, u)
	}
	b.pattern = make([]byte, 8<<20)
	for off := 0; off < len(b.pattern); off += 4096 {
		block := b.pattern[off : off+4096]
		if off > 0 && r.IntN(5) < 3 {
			copy(block, b.pattern[r.IntN(off/4096)*4096:])
		} else {
			for i := range block {
				block[i] = byte(r.Uint32())
			}
		}
	}
	return b
}

// Cold replays a full build in dir/target/debug.
func (b *Build) Cold(dir string) error {
	t := filepath.Join(dir, "target", "debug")
	for _, d := range []string{"deps", "build", ".fingerprint", "incremental", "examples"} {
		if err := os.MkdirAll(filepath.Join(t, d), 0o755); err != nil {
			return err
		}
	}
	if err := b.write(filepath.Join(t, ".cargo-lock"), 0, 0o644); err != nil {
		return err
	}
	for level := range levels {
		var batch []unit
		for _, u := range b.plan {
			if u.level == level {
				batch = append(batch, u)
			}
		}
		if err := parallel(b.jobs, batch, func(u unit) error { return b.compile(t, u) }); err != nil {
			return err
		}
	}
	return b.link(dir)
}

// Relink touches the touch source in dir, then replays the link that cargo
// runs after a one-file change.
func (b *Build) Relink(dir string) error {
	if err := Touch(filepath.Join(dir, b.touch)); err != nil {
		return err
	}
	return b.link(dir)
}

// Clean removes dir/target, as cargo clean does.
func (b *Build) Clean(dir string) error {
	return os.RemoveAll(filepath.Join(dir, "target"))
}

// compile replays one library unit: its build script if any, reading its
// dependencies' metadata, then rustc's outputs and cargo's fingerprint.
func (b *Build) compile(t string, u unit) error {
	if u.script {
		scriptDir := filepath.Join(t, "build", u.name+"-"+u.scriptHash)
		bin := filepath.Join(scriptDir, "build_script_build-"+u.scriptHash)
		runDir := filepath.Join(t, "build", u.name+"-"+u.runHash)
		steps := []func() error{
			func() error { return os.MkdirAll(scriptDir, 0o755) },
			func() error { return b.write(bin+".d", 1<<10, 0o644) },
			func() error { return b.write(bin, 900<<10, 0o755) },
			func() error { return linkOrCopy(bin, filepath.Join(scriptDir, "build-script-build")) },
			func() error { return b.fingerprint(t, "build-script-build-script-build", u.name+"-"+u.scriptHash) },
			func() error { return os.MkdirAll(filepath.Join(runDir, "out"), 0o755) },
			func() error { return b.write(filepath.Join(runDir, "out", "generated.rs"), 40<<10, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "output"), 1<<10, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "root-output"), 100, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "stderr"), 0, 0o644) },
			func() error { return b.write(filepath.Join(runDir, "invoked.timestamp"), 0, 0o644) },
			func() error { return b.fingerprint(t, "run-build-script-build-script-build", u.name+"-"+u.runHash) },
		}
		for _, step := range steps {
			if err := step(); err != nil {
				return err
			}
		}
	}
	deps := filepath.Join(t, "deps")
	for _, d := range u.deps {
		dep := b.plan[d]
		if err := readAll(filepath.Join(deps, "lib"+dep.name+"-"+dep.hash+".rmeta")); err != nil {
			return err
		}
	}
	stem := u.name + "-" + u.hash
	if err := b.write(filepath.Join(deps, stem+".d"), 2<<10, 0o644); err != nil {
		return err
	}
	if err := b.viaTemp(deps, "rmeta"+u.hash[:6], "full.rmeta", filepath.Join(deps, "lib"+stem+".rmeta"), u.rmeta); err != nil {
		return err
	}
	var objects []string
	for k := range 4 {
		o := filepath.Join(deps, fmt.Sprintf("%s.%s.%d-cgu.0.rcgu.o", stem, u.name, k))
		if err := b.write(o, u.rlib/4, 0o644); err != nil {
			return err
		}
		objects = append(objects, o)
	}
	if err := b.viaTemp(deps, "rustc"+u.hash[6:12], "lib"+u.name+".rlib", filepath.Join(deps, "lib"+stem+".rlib"), u.rlib); err != nil {
		return err
	}
	for _, o := range objects {
		if err := os.Remove(o); err != nil {
			return err
		}
	}
	return b.fingerprint(t, "lib-"+u.name, stem)
}

// viaTemp writes size bytes as name inside a fresh temporary directory below
// dir, renames it to dst and removes the directory, as rustc emits outputs.
func (b *Build) viaTemp(dir, tmp, name, dst string, size int) error {
	tmpDir := filepath.Join(dir, tmp)
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		return err
	}
	if err := b.write(filepath.Join(tmpDir, name), size, 0o644); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(tmpDir, name), dst); err != nil {
		return err
	}
	return os.Remove(tmpDir)
}

// fingerprint writes cargo's fingerprint files for one unit.
func (b *Build) fingerprint(t, kind, stem string) error {
	fp := filepath.Join(t, ".fingerprint", stem)
	if err := os.MkdirAll(fp, 0o755); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		size int
	}{{"invoked.timestamp", 0}, {"dep-" + kind, 1 << 10}, {kind, 16}, {kind + ".json", 2 << 10}} {
		if err := b.write(filepath.Join(fp, f.name), f.size, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// link replays the binary crate: rustc reads the sources and direct
// dependencies' metadata and writes codegen objects; the linker reads every
// rlib and object, writes the executable, and cargo hard-links it into place.
func (b *Build) link(dir string) error {
	t := filepath.Join(dir, "target", "debug")
	deps := filepath.Join(t, "deps")
	for _, s := range b.sources {
		if err := readAll(filepath.Join(dir, s)); err != nil {
			return err
		}
	}
	for _, u := range b.plan[len(b.plan)-20:] {
		if err := readAll(filepath.Join(deps, "lib"+u.name+"-"+u.hash+".rmeta")); err != nil {
			return err
		}
	}
	stem := BinName + "-" + binHash
	if err := b.write(filepath.Join(deps, stem+".d"), 5<<10, 0o644); err != nil {
		return err
	}
	objects := make([]int, binObjects)
	for i := range objects {
		objects[i] = i
	}
	err := parallel(b.jobs, objects, func(i int) error {
		return b.write(filepath.Join(deps, fmt.Sprintf("%s.%s.%04d.rcgu.o", stem, BinName, i)), binObject, 0o644)
	})
	if err != nil {
		return err
	}
	for _, u := range b.plan {
		if err := readAll(filepath.Join(deps, "lib"+u.name+"-"+u.hash+".rlib")); err != nil {
			return err
		}
	}
	for i := range binObjects {
		if err := readAll(filepath.Join(deps, fmt.Sprintf("%s.%s.%04d.rcgu.o", stem, BinName, i))); err != nil {
			return err
		}
	}
	exe := filepath.Join(deps, stem)
	if err := b.write(exe, binSize, 0o755); err != nil {
		return err
	}
	final := filepath.Join(t, BinName)
	if err := os.Remove(final); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := linkOrCopy(exe, final); err != nil {
		return err
	}
	if err := b.write(final+".d", 1<<10, 0o644); err != nil {
		return err
	}
	return b.fingerprint(t, "bin-"+BinName, stem)
}

// linkOrCopy hard-links src to dst and, when the filesystem refuses, copies
// src with its permission bits instead, mirroring cargo's link-or-copy so the
// replay also runs where hard links are unsupported.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	buf := readBufs.Get().(*[]byte)
	defer readBufs.Put(buf)
	_, err = io.CopyBuffer(out, struct{ io.Reader }{in}, *buf)
	return errors.Join(err, out.Close())
}

// Noop replays a no-op build's checks: every fingerprint and dep-info file
// read, every output and source stat'ed.
func (b *Build) Noop(dir string) error {
	t := filepath.Join(dir, "target", "debug")
	err := filepath.WalkDir(filepath.Join(t, ".fingerprint"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return readAll(p)
	})
	if err != nil {
		return err
	}
	deps := filepath.Join(t, "deps")
	for _, u := range b.plan {
		stem := u.name + "-" + u.hash
		if err := readAll(filepath.Join(deps, stem+".d")); err != nil {
			return err
		}
		for _, p := range []string{"lib" + stem + ".rlib", "lib" + stem + ".rmeta"} {
			if _, err := os.Stat(filepath.Join(deps, p)); err != nil {
				return err
			}
		}
		if u.script {
			if err := readAll(filepath.Join(t, "build", u.name+"-"+u.runHash, "output")); err != nil {
				return err
			}
		}
	}
	for _, s := range b.sources {
		if _, err := os.Lstat(filepath.Join(dir, s)); err != nil {
			return err
		}
	}
	_, err = os.Stat(filepath.Join(t, BinName))
	return err
}

// write creates path with size bytes of the pattern, in 64 KiB writes like a
// buffered writer.
func (b *Build) write(path string, size int, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	for off := 0; off < size; {
		n := min(64<<10, size-off)
		start := off % (len(b.pattern) - n)
		if _, err := f.Write(b.pattern[start : start+n]); err != nil {
			f.Close()
			return err
		}
		off += n
	}
	return f.Close()
}

// readBufs recycles the buffers readAll copies through.
var readBufs = sync.Pool{New: func() any { b := make([]byte, 256<<10); return &b }}

// readAll reads the file at path to its end, discarding the data, as a
// compiler or linker reading an input does.
func readAll(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := readBufs.Get().(*[]byte)
	defer readBufs.Put(buf)
	_, err = io.CopyBuffer(io.Discard, struct{ io.Reader }{f}, *buf)
	return err
}

// Touch sets path's modification time to now, as editing a source does.
func Touch(path string) error {
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// parallel runs fn over items with at most n at once, returning the first error.
func parallel[T any](n int, items []T, fn func(T) error) error {
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		slots = make(chan struct{}, n)
	)
	for _, item := range items {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := fn(item); err != nil {
				once.Do(func() { first = err })
			}
		})
	}
	wg.Wait()
	return first
}
