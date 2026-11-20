// Package fixture builds the git repository that build benchmarks run on: a
// cargo crate plus many bulk files that make the repository realistically
// large, and copies of the crate alone for the plain-directory baseline.
package fixture

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/stencil-hq/ovnfs/scripts/buildbench/replay"
)

// Repo is a fixture repository and the crate sources a build of it reads.
type Repo struct {
	Dir     string   // repository root
	Sources []string // crate-relative files below src/, sorted
	Touch   string   // crate-relative source touched for incremental phases
}

// Make builds the fixture repository in dir: the crate at its root
// (project's tracked and untracked-unignored files, or a synthetic crate) and
// files bulk files below bulk/. One git fast-import stream commits everything
// and a parallel checkout-index writes the work tree with a clean index:
// seconds, where writing the files and running git add takes a minute per
// 100,000.
func Make(dir, project string, files int) (*Repo, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := git(dir, "init", "-q", "-b", "main"); err != nil {
		return nil, err
	}
	imp := exec.Command("git", "fast-import", "--quiet")
	imp.Dir = dir
	var stderr bytes.Buffer
	imp.Stderr = &stderr
	in, err := imp.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := imp.Start(); err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(in, 1<<20)
	type entry struct {
		path, mode string
		mark       int
	}
	var entries []entry
	add := func(path, mode string, data []byte) {
		entries = append(entries, entry{path, mode, len(entries) + 1})
		fmt.Fprintf(w, "blob\nmark :%d\ndata %d\n", len(entries), len(data))
		w.Write(data)
		w.WriteByte('\n')
	}
	if project != "" {
		err = addProject(project, add)
	} else {
		addSyntheticCrate(add)
	}
	if err != nil {
		imp.Process.Kill()
		imp.Wait()
		return nil, err
	}
	for i := range files {
		add(fmt.Sprintf("bulk/d%03d/f%05d.txt", i/250, i), "100644", fmt.Appendf(nil, "bulk file %d\n", i))
	}
	repo := &Repo{Dir: dir}
	fmt.Fprintf(w, "commit refs/heads/main\ncommitter buildbench <buildbench@localhost> 0 +0000\ndata 7\nfixture\n")
	for _, e := range entries {
		path := e.path
		if strings.ContainsAny(path, "\n\"") {
			path = strconv.Quote(path)
		}
		fmt.Fprintf(w, "M %s :%d %s\n", e.mode, e.mark, path)
		if strings.HasPrefix(e.path, "src/") {
			repo.Sources = append(repo.Sources, filepath.FromSlash(e.path))
		}
	}
	if err := errors.Join(w.Flush(), in.Close()); err != nil {
		imp.Wait()
		return nil, fmt.Errorf("git fast-import: %w\n%s", err, stderr.Bytes())
	}
	if err := imp.Wait(); err != nil {
		return nil, fmt.Errorf("git fast-import: %w\n%s", err, stderr.Bytes())
	}
	if err := git(dir, "read-tree", "main"); err != nil {
		return nil, err
	}
	if err := git(dir, "-c", "checkout.workers="+strconv.Itoa(runtime.NumCPU()), "checkout-index", "-a", "-u"); err != nil {
		return nil, err
	}
	if len(repo.Sources) == 0 {
		return nil, errors.New("crate has no files below src/")
	}
	slices.Sort(repo.Sources)
	repo.Touch = repo.Sources[0]
	for _, main := range []string{"src/main.rs", "src/lib.rs"} {
		if slices.Contains(repo.Sources, filepath.FromSlash(main)) {
			repo.Touch = filepath.FromSlash(main)
			break
		}
	}
	return repo, nil
}

// git runs git with args in dir, reporting its output on failure.
func git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", args[0], err, out)
	}
	return nil
}

// addProject passes project's tracked and untracked-unignored regular files
// and symlinks to add, with git's modes; submodules and deleted paths are skipped.
func addProject(project string, add func(path, mode string, data []byte)) error {
	out, err := exec.Command("git", "-C", project, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		return fmt.Errorf("git ls-files %s: %w", project, err)
	}
	for rel := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		p := filepath.Join(project, rel)
		info, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return err
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			add(rel, "120000", []byte(target))
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			mode := "100644"
			if info.Mode()&0o111 != 0 {
				mode = "100755"
			}
			add(rel, mode, data)
		}
	}
	return nil
}

// CopyCrate copies dir's crate, everything but bulk/ and .git, to dst.
func CopyCrate(dir, dst string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "bulk" || rel == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		return copyEntry(p, filepath.Join(dst, rel))
	})
}

// copyEntry copies one regular file or symlink, skipping anything else.
func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.Mode().IsRegular():
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, info.Mode().Perm())
	}
	return nil
}

// addSyntheticCrate passes a dependency-free binary crate of 100 modules to add.
func addSyntheticCrate(add func(path, mode string, data []byte)) {
	add("Cargo.toml", "100644", []byte("[package]\nname = \""+replay.BinName+"\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"))
	var main strings.Builder
	for i := range 100 {
		fmt.Fprintf(&main, "mod m%03d;\n", i)
		var m strings.Builder
		for j := range 40 {
			fmt.Fprintf(&m, "pub fn f%d(x: u64) -> u64 { x.wrapping_mul(%d).rotate_left(%d) ^ %d }\n", j, i*40+j+1, j%63, i)
		}
		add(fmt.Sprintf("src/m%03d.rs", i), "100644", []byte(m.String()))
	}
	main.WriteString("fn main() {\n    let mut x = 1u64;\n")
	for i := range 100 {
		fmt.Fprintf(&main, "    x = m%03d::f%d(x);\n", i, i%40)
	}
	main.WriteString("    println!(\"{x}\");\n}\n")
	add("src/main.rs", "100644", []byte(main.String()))
}
