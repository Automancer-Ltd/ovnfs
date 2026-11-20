package base

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stencil-hq/ovnfs/rules"
	"github.com/stencil-hq/ovnfs/source"
	"github.com/stencil-hq/ovnfs/store"
)

// TestRevisionIndexCarriesPresentedStat seeds the --rev index with exactly the
// size and mtime the view presents for REV's content, so git trusts those files
// unread, while taken paths, gitlinks and racily clean files are still read.
func TestRevisionIndexCarriesPresentedStat(t *testing.T) {
	ctx := context.Background()
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, "rev-only.txt"), "revision\n")
	writeTestFile(t, filepath.Join(dir, "taken.txt"), "committed\n")
	writeTestFile(t, filepath.Join(dir, "future.txt"), "future\n")
	if runtime.GOOS != "windows" {
		if err := os.Symlink("source.txt", filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+runGit(t, dir, "rev-parse", "HEAD")+",sub")
	runGit(t, dir, "commit", "-qm", "revision")
	revision, err := repo.ResolveRevision(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	// The folder then drifts from REV: one file differs, one is staged for a
	// take, and one carries an mtime later than any index write.
	writeTestFile(t, filepath.Join(dir, "rev-only.txt"), "folder edit\n")
	writeTestFile(t, filepath.Join(dir, "taken.txt"), "staged taken\n")
	runGit(t, dir, "add", "taken.txt")
	past := time.Unix(1_700_000_000, 123456789)
	for _, p := range []string{"source.txt", "src/nested.txt"} {
		if err := os.Chtimes(filepath.Join(dir, filepath.FromSlash(p)), past, past); err != nil {
			t.Fatal(err)
		}
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "future.txt"), future, future); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ID: "b_rev", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
	before := time.Now()
	b, _, err := CreateRevision(ctx, c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}, revision, Take{Path: "taken.txt"})
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	name := filepath.Join(c.StateDir, "revision-index")
	idx, err := repo.ReadIndex(name)
	if err != nil {
		t.Fatal(err)
	}
	var seeded []string
	for _, e := range idx.Entries {
		zero := e.Size == 0 && e.Mtime.Equal(time.Unix(0, 0))
		if e.Path == "taken.txt" || e.Path == "sub" {
			if !zero {
				t.Errorf("%s: stat %d %s, want zero: its content is not REV's", e.Path, e.Size, e.Mtime)
			}
			continue
		}
		info, err := b.Bottom(nil).Lstat(e.Path)
		if err != nil {
			t.Fatal(err)
		}
		mtime := info.ModTime()
		switch {
		case !e.Mtime.Equal(mtime):
			t.Errorf("%s: seeded mtime %s, presented %s", e.Path, e.Mtime, mtime)
		case mtime.Unix() < before.Unix() && int64(e.Size) != info.Size():
			t.Errorf("%s: seeded size %d, presented %d", e.Path, e.Size, info.Size())
		case mtime.Unix() > after.Unix() && e.Size != 0:
			t.Errorf("%s: racily clean entry keeps size %d", e.Path, e.Size)
		case e.Size != 0 && int64(e.Size) != info.Size():
			t.Errorf("%s: seeded size %d, presented %d", e.Path, e.Size, info.Size())
		}
		seeded = append(seeded, e.Path)
	}
	want := []string{".gitignore", "future.txt", "rev-only.txt", "source.txt", "src/nested.txt"}
	if runtime.GOOS != "windows" {
		want = append(want, "link")
		slices.Sort(want)
	}
	if !slices.Equal(seeded, want) {
		t.Fatalf("seeded %v, want %v", seeded, want)
	}
	for _, e := range idx.Entries {
		if fork := b.Meta().ForkTime; e.Path == "rev-only.txt" && !e.Mtime.Equal(fork) {
			t.Fatalf("content the folder never held is seeded with %s, not the fork time %s", e.Mtime, fork)
		}
	}

	// Git trusts the seeded stat data without reading: a same-size edit that
	// keeps the presented mtime goes unnoticed, while racily clean and taken
	// entries are read and compared.
	work := t.TempDir()
	for _, e := range idx.Entries {
		host := filepath.Join(work, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
			t.Fatal(err)
		}
		switch e.Path {
		case "sub":
			if err := os.Mkdir(host, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		case "link":
			if err := os.Symlink("source.txt", host); err != nil {
				t.Fatal(err)
			}
			continue
		}
		content := readBase(t, b, e.Path)
		if e.Path == "source.txt" || e.Path == "future.txt" {
			content = strings.ToUpper(content)
		}
		writeTestFile(t, host, content)
		info, err := b.Bottom(nil).Lstat(e.Path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(host, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "--git-dir", filepath.Join(dir, ".git"), "--work-tree", work,
		"-c", "core.checkStat=minimal", "-c", "core.trustctime=false", "status", "--porcelain")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_INDEX_FILE="+name, "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if got := strings.TrimRight(string(out), "\n"); got != " M future.txt\n M taken.txt" {
		t.Fatalf("git status = %q, want only the racily clean and taken files read and changed", got)
	}
}

// TestRevisionServesLFSObjects shows files the lfs filter tracks as git's
// checkout writes them: the object a pointer names, from the repository's LFS
// storage or else produced by git lfs smudge, while the tree and the revision
// index keep the pointer git stores. Files without the filter, and takes from
// the work tree, keep their bytes.
func TestRevisionServesLFSObjects(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "")
	repo, dir := repository(t)
	pointer := func(object string) string {
		return fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n", sha256.Sum256([]byte(object)), len(object))
	}
	const local, fetched = "object in the LFS storage\n", "object git lfs smudge fetched\n"
	writeTestFile(t, filepath.Join(dir, ".gitattributes"), "*.bin filter=lfs -text\n")
	for p, content := range map[string]string{
		"local.bin": pointer(local), "fetched.bin": pointer(fetched), "wrong.bin": pointer("right object"),
		"taken.bin": pointer(local), "notes.txt": pointer(local),
		"legacy.bin": strings.Replace(pointer(local), "git-lfs.github.com", "hawser.github.com", 1),
	} {
		writeTestFile(t, filepath.Join(dir, p), content)
	}
	// The filter is configured only once the pointers are committed as they are.
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "commit", "-qm", "lfs")
	runGit(t, dir, "config", "filter.lfs.process", "git-lfs filter-process")
	// A dirty .gitattributes decides nothing: REV's and the index's apply.
	writeTestFile(t, filepath.Join(dir, ".gitattributes"), "")
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(local)))
	writeTestFile(t, filepath.Join(dir, ".git", "lfs", "objects", sum[:2], sum[2:4], sum), local)
	// A stand-in git-lfs: git finds it in its exec path before PATH.
	smudgeScript := filepath.Join(t.TempDir(), "git-lfs")
	writeTestFile(t, smudgeScript, "#!/bin/sh\ncat >/dev/null\ncase \"$3\" in\nfetched.bin) printf '%s' '"+fetched+"' ;;\n*) printf 'wrong object' ;;\nesac\n")
	if err := os.Chmod(smudgeScript, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_EXEC_PATH", filepath.Dir(smudgeScript))
	revision, err := repo.ResolveRevision(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	var faults []string
	c := Config{ID: "b_rev", StateDir: filepath.Join(t.TempDir(), "base"), Store: st, OnFault: func(msg string) { faults = append(faults, msg) }}
	b, _, err := CreateRevision(ctx, c, source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}, revision,
		Take{Path: "local.bin"}, Take{Source: ".", Path: "taken.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for p, want := range map[string]string{"local.bin": local, "taken.bin": pointer(local), "notes.txt": pointer(local)} {
		info, err := b.Bottom(nil).Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := readBase(t, b, p); got != want || info.Size() != int64(len(want)) {
			t.Errorf("%s = %q (%d bytes listed), want %q", p, got, info.Size(), want)
		}
	}
	// git-lfs rewrites a legacy pointer when cleaning: git must read it, so
	// the index holds no stat data for it.
	if info, err := b.Bottom(nil).Lstat("legacy.bin"); err != nil || info.Size() != int64(len(local)) || !info.ModTime().Equal(b.Meta().ForkTime) {
		t.Errorf("legacy.bin = %v, %v; want its object at the fork time", info, err)
	}
	if leaf, _ := b.Leaf("local.bin"); string(leaf.ID) != runGit(t, dir, "rev-parse", "HEAD:local.bin") {
		t.Errorf("tree holds %s for local.bin, not the pointer git stores", leaf.ID)
	}
	idx, err := repo.ReadIndex(filepath.Join(c.StateDir, revisionIndex))
	if err != nil {
		t.Fatal(err)
	}
	info, err := b.Bottom(nil).Lstat("local.bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range idx.Entries {
		if e.Path == "local.bin" && (int64(e.Size) != info.Size() || !e.Mtime.Equal(info.ModTime())) {
			t.Errorf("index seeds local.bin with %d bytes at %s, the view shows %d at %s", e.Size, e.Mtime, info.Size(), info.ModTime())
		}
		if e.Path == "legacy.bin" && (e.Size != 0 || !e.Mtime.Equal(time.Unix(0, 0))) {
			t.Errorf("index seeds legacy.bin with %d bytes at %s, want no stat data", e.Size, e.Mtime)
		}
	}

	if runtime.GOOS == "windows" {
		return // the stand-in git-lfs is a shell script
	}
	if got := readBase(t, b, "fetched.bin"); got != fetched {
		t.Errorf("fetched.bin = %q, want %q from git lfs smudge", got, fetched)
	}
	// Output of the promised size that is not the object (as git-lfs writes
	// the pointer back for a failed download) is no object.
	_, err = b.Bottom(nil).Open("wrong.bin")
	if !errors.Is(err, syscall.EIO) || len(faults) != 1 || !strings.Contains(faults[0], "wrong.bin") {
		t.Errorf("other content than the object opened with %v, faults %q; want EIO and one fault", err, faults)
	}
	// A produced object is kept: git lfs is not asked again.
	writeTestFile(t, smudgeScript, "#!/bin/sh\nexit 1\n")
	if got := readBase(t, b, "fetched.bin"); got != fetched {
		t.Errorf("fetched.bin reread = %q, want %q", got, fetched)
	}
}

// TestRevisionHiddenRootsStayHidden keeps hidden roots out of a --rev view,
// including tracked paths a take would otherwise bring back.
func TestRevisionHiddenRootsStayHidden(t *testing.T) {
	ctx := context.Background()
	repo, dir := repository(t)
	writeTestFile(t, filepath.Join(dir, ".gitignore"), "tmp/\n.DS_Store\n*.log\n")
	writeTestFile(t, filepath.Join(dir, "vendor", "lib.txt"), "vendored\n")
	runGit(t, dir, "add", "--all")
	runGit(t, dir, "commit", "-qm", "ignores")
	revision, err := repo.ResolveRevision(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	for p, content := range map[string]string{
		"tmp/copy/repo/file.txt": "scratch copy\n",
		".DS_Store":              "finder\n",
		"src/.DS_Store":          "finder\n",
		"notes/.DS_Store":        "finder\n",
		"vendor/build.log":       "ignored log\n",
		"vendor/new.txt":         "untracked\n",
	} {
		writeTestFile(t, filepath.Join(dir, filepath.FromSlash(p)), content)
	}
	always := []string{".DS_Store", "src/.DS_Store", "notes", "vendor/new.txt"}
	for _, tc := range []struct {
		name   string
		rules  []string
		takes  []Take
		absent []string
		cached map[string]string
	}{
		{name: "builtins", cached: map[string]string{"tmp/copy/repo/file.txt": "scratch copy\n", "vendor/build.log": "ignored log\n"}},
		{name: "hidden", rules: []string{"hidden tmp/", "hidden vendor/"}, absent: []string{"tmp", "vendor", "vendor/lib.txt", "vendor/build.log"}},
		{name: "hidden with take", rules: []string{"hidden tmp/", "hidden vendor/"}, takes: []Take{{Source: "HEAD", Revision: revision}}, absent: []string{"tmp", "vendor", "vendor/lib.txt", "vendor/build.log"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "store"), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			c := Config{ID: "b_rev", StateDir: filepath.Join(t.TempDir(), "base"), Store: st}
			o := source.Options{Root: repo.Top, Rules: rules.Set{Rules: rules.Builtins()}}
			for _, line := range tc.rules {
				prepend(t, &o, line)
			}
			b, _, err := CreateRevision(ctx, c, o, revision, tc.takes...)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			for _, p := range append(slices.Clone(always), tc.absent...) {
				if info, err := b.Bottom(nil).Lstat(p); !os.IsNotExist(err) {
					t.Errorf("%s = %v, %v; want absent", p, info, err)
				}
			}
			for p, want := range tc.cached {
				if got := readBase(t, b, p); got != want {
					t.Errorf("%s = %q, want %q", p, got, want)
				}
			}
			if tc.cached != nil && readBase(t, b, "vendor/lib.txt") != "vendored\n" {
				t.Error("tracked file without a hidden rule did not survive")
			}
			list, err := b.Bottom(b.TakeCaches("c1")).ReadDir("")
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, info := range list {
				names = append(names, info.Name())
			}
			wantNames := []string{".gitignore", "source.txt", "src"}
			if tc.cached != nil {
				wantNames = []string{".gitignore", "source.txt", "src", "tmp", "vendor"}
			}
			if !slices.Equal(names, wantNames) {
				t.Errorf("root lists %v, want %v", names, wantNames)
			}
			if tc.cached == nil {
				for _, p := range []string{"tmp", "vendor"} {
					if class, _ := b.Class(p); class != rules.Hidden {
						t.Errorf("%s recorded as %q, want a hidden root", p, class)
					}
				}
				if !b.Domains().Facts(source.Dir(dir), "vendor/lib.txt", nil).Tracked() {
					t.Error("hidden revision file lost its tracked status")
				}
			}
		})
	}
}
