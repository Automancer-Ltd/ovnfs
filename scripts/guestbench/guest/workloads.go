//go:build linux

package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/stencil-hq/ovnfs/scripts/buildbench/replay"
)

// Shape of the small workload: smallFiles files of smallSize bytes spread
// evenly over smallDirs directories.
const (
	smallFiles = 10000
	smallDirs  = 100
	smallSize  = 1 << 10
)

// rgPattern is what the rg workload searches for: the fixture's bulk files
// hold "bulk file N", so it matches a fraction of them.
const rgPattern = "bulk file 1"

// coldWarm runs body as phase cold after a cold reset, then again as phase
// warm on the caches the first run filled.
func (r *runner) coldWarm(round int, cold, warm string, body func() (result, error)) error {
	return r.phases(round, []phase{
		{name: cold, cold: true, body: body},
		{name: warm, body: body},
	})
}

// walkRounds times an lstat of every entry of the tree, cold then warm; it
// also counts the tree's files for the rg workload.
func (r *runner) walkRounds() (func(round int) error, error) {
	body := func() (result, error) {
		entries, files, err := walkTree(r.root)
		r.files = files
		return result{ops: entries}, err
	}
	return func(round int) error {
		return r.coldWarm(round, "walk.cold", "walk.warm", body)
	}, nil
}

// walkTree lstats every entry below root, skipping nothing, and returns the
// entries visited and the regular files among them.
func walkTree(root string) (entries, files int64, err error) {
	err = filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if _, err := d.Info(); err != nil {
			return err
		}
		entries++
		if d.Type().IsRegular() {
			files++
		}
		return nil
	})
	return entries, files, err
}

// gitRounds times `git status` in the tree, cold then warm; its Ops is the
// index's entry count, the files git compares with the work tree.
func (r *runner) gitRounds() (func(round int) error, error) {
	out, err := runCmd(r.gitCmd("ls-files", "-z"))
	if err != nil {
		return nil, err
	}
	entries := int64(bytes.Count(out, []byte{0}))
	body := func() (result, error) {
		_, err := runCmd(r.gitCmd("status", "--porcelain"))
		return result{ops: entries}, err
	}
	return func(round int) error {
		return r.coldWarm(round, "git.cold", "git.warm", body)
	}, nil
}

// rgRounds times ripgrep over every file of the tree, cold then warm; its
// Ops is the walk's file count when the walk ran.
func (r *runner) rgRounds() (func(round int) error, error) {
	args := []string{"-c", "--no-messages", "--hidden", "-g", "!.git", rgPattern, r.root}
	body := func() (result, error) {
		// Exit status 1 only means no file matched.
		_, err := runCmd(command(r.root, "rg", args...), 1)
		return result{ops: r.files}, err
	}
	return func(round int) error {
		return r.coldWarm(round, "rg.cold", "rg.warm", body)
	}, nil
}

// smallSet holds the small workload's paths, computed outside the timing.
type smallSet struct {
	dirs    []string   // scratch/dNN
	created [][]string // per directory, its files as created
	renamed [][]string // per directory, its files after the rename
}

// smallRounds times creating, stat'ing, renaming and unlinking many small
// files in the tree's scratch directory, which each round removes again.
// A syncfs after the creates and another after the unlinks time the work
// those phases left undone: ext4's writeback, a network client's flush, the
// module's journal.
func (r *runner) smallRounds() (func(round int) error, error) {
	var s smallSet
	for d := range smallDirs {
		dir := filepath.Join(r.scratch(), fmt.Sprintf("d%02d", d))
		var created, renamed []string
		for f := range smallFiles / smallDirs {
			created = append(created, filepath.Join(dir, fmt.Sprintf("f%03d", f)))
			renamed = append(renamed, filepath.Join(dir, fmt.Sprintf("r%03d", f)))
		}
		s.dirs = append(s.dirs, dir)
		s.created = append(s.created, created)
		s.renamed = append(s.renamed, renamed)
	}
	data := bytes.Repeat([]byte{'s'}, smallSize)
	jobs := r.plan.Jobs
	each := func(fn func(d int) error) func() (result, error) {
		return func() (result, error) {
			return result{ops: smallFiles}, parallel(jobs, smallDirs, fn)
		}
	}
	fresh := func() error {
		if err := os.RemoveAll(r.scratch()); err != nil {
			return err
		}
		return os.Mkdir(r.scratch(), 0o755)
	}
	create := func(d int) error {
		if err := os.Mkdir(s.dirs[d], 0o755); err != nil {
			return err
		}
		for _, p := range s.created[d] {
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	stat := func(d int) error {
		for _, p := range s.created[d] {
			if _, err := os.Lstat(p); err != nil {
				return err
			}
		}
		return nil
	}
	rename := func(d int) error {
		for i, p := range s.created[d] {
			if err := os.Rename(p, s.renamed[d][i]); err != nil {
				return err
			}
		}
		return nil
	}
	unlink := func(d int) error {
		for _, p := range s.renamed[d] {
			if err := os.Remove(p); err != nil {
				return err
			}
		}
		return os.Remove(s.dirs[d])
	}
	settle := func() (result, error) { return result{ops: smallFiles}, r.syncTree() }
	return func(round int) error {
		if err := r.phases(round, []phase{
			{name: "small.create", before: fresh, body: each(create)},
			{name: "small.sync", body: settle},
			{name: "small.stat", body: each(stat)},
			{name: "small.rename", body: each(rename)},
			{name: "small.unlink", body: each(unlink)},
			{name: "small.settle", body: settle},
		}); err != nil {
			return err
		}
		return os.RemoveAll(r.scratch())
	}, nil
}

// parallel calls fn for every index below n on at most jobs goroutines and
// returns the first error; a worker stops at its first error.
func parallel(jobs, n int, fn func(i int) error) error {
	var (
		wg    sync.WaitGroup
		next  atomic.Int64
		once  sync.Once
		first error
	)
	for range min(jobs, n) {
		wg.Go(func() {
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				if err := fn(i); err != nil {
					once.Do(func() { first = err })
					return
				}
			}
		})
	}
	wg.Wait()
	return first
}

// inTree adapts a function of the tree's directory to a phase body that
// measures nothing but time.
func (r *runner) inTree(fn func(dir string) error) func() (result, error) {
	return func() (result, error) { return result{}, fn(r.root) }
}

// replayRounds times cargo's filesystem traffic without compilers: a cold
// build from a removed target directory, a no-op build, a relink after a
// touch and a clean, which leaves no target behind.
func (r *runner) replayRounds() (func(round int) error, error) {
	b := replay.New(r.plan.Jobs, r.plan.Sources, r.plan.Touch)
	return func(round int) error {
		return r.phases(round, []phase{
			{name: "replay.cold", before: r.removeTarget, cold: true, body: r.inTree(b.Cold)},
			{name: "replay.no-op", body: r.inTree(b.Noop)},
			{name: "replay.touch", body: r.inTree(b.Relink)},
			{name: "replay.clean", body: r.inTree(b.Clean)},
		})
	}, nil
}

// cargoRounds times `cargo build --offline` of the tree's crate: from a
// removed target directory in the first Plan.Colds rounds, then a no-op
// build and a build after touching one source in every round, each on the
// target the previous build left. Without cold rounds an untimed build
// provides that target.
func (r *runner) cargoRounds() (func(round int) error, error) {
	build := func(dir string) error {
		_, err := runCmd(command(dir, "cargo", "build", "--offline"))
		return err
	}
	if r.plan.Colds <= 0 {
		if err := r.removeTarget(); err != nil {
			return nil, err
		}
		if err := build(r.root); err != nil {
			return nil, err
		}
	}
	touch := func() error { return replay.Touch(filepath.Join(r.root, r.plan.Touch)) }
	return func(round int) error {
		var ps []phase
		if round < r.plan.Colds {
			ps = append(ps, phase{name: "cargo.cold", before: r.removeTarget, cold: true, body: r.inTree(build)})
		}
		ps = append(ps,
			phase{name: "cargo.no-op", body: r.inTree(build)},
			phase{name: "cargo.touch", before: touch, body: r.inTree(build)},
		)
		return r.phases(round, ps)
	}, nil
}
