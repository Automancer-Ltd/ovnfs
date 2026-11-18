package git

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseLFSPointer(t *testing.T) {
	const oid = "6075562b771f8b82f0c179e363389684f2dd09de30038269e2628e504bd7be0f"
	const version = "version https://git-lfs.github.com/spec/v1\n"
	for _, tc := range []struct {
		name, data string
		want       LFSPointer
		ok         bool
	}{
		{"pointer", version + "oid sha256:" + oid + "\nsize 733736\n", LFSPointer{OID: oid, Size: 733736}, true},
		{"hawser version", "version https://hawser.github.com/spec/v1\noid sha256:" + oid + "\nsize 0\n", LFSPointer{OID: oid}, true},
		{"git-media version", "version http://git-media.io/v/2\noid sha256:" + oid + "\nsize 2\n", LFSPointer{OID: oid, Size: 2}, true},
		{"crlf, blank lines, no final newline", "\r\nversion https://git-lfs.github.com/spec/v1\r\n\r\noid sha256:" + oid + "\r\nsize 1", LFSPointer{OID: oid, Size: 1}, true},
		{"unknown version", "version https://git-lfs.github.com/spec/v2\noid sha256:" + oid + "\nsize 1\n", LFSPointer{}, false},
		{"extension", version + "ext-0-foo sha256:" + oid + "\noid sha256:" + oid + "\nsize 1\n", LFSPointer{}, false},
		{"trailing line", version + "oid sha256:" + oid + "\nsize 1\nx y\n", LFSPointer{}, false},
		{"keys out of order", version + "size 1\noid sha256:" + oid + "\n", LFSPointer{}, false},
		{"uppercase oid", version + "oid sha256:" + strings.ToUpper(oid) + "\nsize 1\n", LFSPointer{}, false},
		{"short oid", version + "oid sha256:" + oid[1:] + "\nsize 1\n", LFSPointer{}, false},
		{"negative size", version + "oid sha256:" + oid + "\nsize -1\n", LFSPointer{}, false},
		{"pointer-sized content", version + "oid sha256:" + oid + "\nsize 1\n" + strings.Repeat("x", LFSPointerLimit), LFSPointer{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseLFSPointer([]byte(tc.data))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseLFSPointer = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestLFSFilter decides as git and git-lfs do (checked against git-lfs 3) which
// pointers checkout smudges: filter.lfs.process wins over filter.lfs.smudge and
// must run git-lfs's own subcommand without its skip flag, GIT_LFS_SKIP_SMUDGE
// takes git-lfs booleans, and lfs.fetchinclude and lfs.fetchexclude, from
// git's configuration or else .lfsconfig (work tree, index, HEAD), select
// paths, case-insensitively on macOS and Windows.
func TestLFSFilter(t *testing.T) {
	paths := []string{"a.bin", "assets/big/b.bin", "assets/c.png", "docs/d.png"}
	folded := []string{"assets/c.png", "docs/d.png"} // what *.BIN leaves where case folds
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		folded = paths
	}
	process := "git-lfs filter-process"
	for _, tc := range []struct {
		name, env, lfsconfig string
		committed            bool // the .lfsconfig is only in HEAD and the index
		config               map[string]string
		want                 []string // the paths smudged; nil when nothing is
	}{
		{name: "no driver"},
		{name: "another program", config: map[string]string{"filter.lfs.smudge": "cat"}},
		{name: "clean command", config: map[string]string{"filter.lfs.smudge": "git-lfs clean -- %f"}},
		{name: "smudge", config: map[string]string{"filter.lfs.smudge": "git-lfs smudge -- %f"}, want: paths},
		{name: "skip=true", config: map[string]string{"filter.lfs.smudge": "git-lfs smudge --skip=true -- %f"}},
		{name: "quoted skip", config: map[string]string{"filter.lfs.smudge": `git-lfs smudge "--skip" -- %f`}},
		{name: "short skip", config: map[string]string{"filter.lfs.process": "git-lfs filter-process -s"}},
		{name: "short skip=true", config: map[string]string{"filter.lfs.smudge": "git-lfs smudge -s=true -- %f"}},
		{name: "skip=false", config: map[string]string{"filter.lfs.smudge": "git-lfs smudge --skip=false -- %f"}, want: paths},
		{name: "case and slash", config: map[string]string{"filter.lfs.process": process, "lfs.fetchexclude": "*.BIN/"}, want: folded},
		{name: "committed lfsconfig", lfsconfig: "[lfs]\n\tfetchexclude = *.png\n", committed: true, config: map[string]string{"filter.lfs.process": process},
			want: []string{"a.bin", "assets/big/b.bin"}},
		{name: "process path", config: map[string]string{"filter.lfs.process": `"C:\Program Files\Git\mingw64\bin\git-lfs.exe" filter-process`}, want: paths},
		{name: "git lfs", config: map[string]string{"filter.lfs.process": "git lfs filter-process"}, want: paths},
		{name: "process skips", config: map[string]string{"filter.lfs.process": "git-lfs filter-process --skip", "filter.lfs.smudge": "git-lfs smudge -- %f"}},
		{name: "process wins", config: map[string]string{"filter.lfs.process": "git-lfs filter-process", "filter.lfs.smudge": "git-lfs smudge --skip -- %f"}, want: paths},
		{name: "env skips", env: "t", config: map[string]string{"filter.lfs.process": "git-lfs filter-process"}},
		{name: "env not a git-lfs true", env: "2", config: map[string]string{"filter.lfs.process": "git-lfs filter-process"}, want: paths},
		{name: "include and exclude", config: map[string]string{"filter.lfs.process": "git-lfs filter-process",
			"lfs.fetchinclude": "assets, *.bin", "lfs.fetchexclude": "big/"}, want: []string{"a.bin", "assets/c.png"}},
		{name: "lfsconfig", lfsconfig: "[lfs]\n\tfetchexclude = *.png\n", config: map[string]string{"filter.lfs.process": "git-lfs filter-process"},
			want: []string{"a.bin", "assets/big/b.bin"}},
		{name: "git config over lfsconfig", lfsconfig: "[lfs]\n\tfetchexclude = *.png\n",
			config: map[string]string{"filter.lfs.process": "git-lfs filter-process", "lfs.fetchexclude": "docs"}, want: []string{"a.bin", "assets/big/b.bin", "assets/c.png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRepo(t)
			t.Setenv("GIT_LFS_SKIP_SMUDGE", tc.env)
			for key, value := range tc.config {
				testGit(t, r.Top, "config", key, value)
			}
			if tc.lfsconfig != "" {
				testWrite(t, filepath.Join(r.Top, ".lfsconfig"), tc.lfsconfig)
			}
			if tc.committed {
				testGit(t, r.Top, "add", ".lfsconfig")
				testGit(t, r.Top, "commit", "-qm", "lfsconfig")
				if err := os.Remove(filepath.Join(r.Top, ".lfsconfig")); err != nil {
					t.Fatal(err)
				}
			}
			filter, err := r.LFSFilter()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, p := range paths {
				if filter != nil && filter.Smudges(p) {
					got = append(got, p)
				}
			}
			if !slices.Equal(got, tc.want) || (filter == nil) != (tc.want == nil) {
				t.Fatalf("smudged %q (filter %v), want %q", got, filter != nil, tc.want)
			}
		})
	}
}

// TestSmudgeLFSCancel returns once its context ends, though git-lfs (a
// grandchild, behind git) hangs on.
func TestSmudgeLFSCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in git-lfs is a shell script")
	}
	r := testRepo(t)
	helper := filepath.Join(t.TempDir(), "git-lfs")
	testWrite(t, helper, "#!/bin/sh\nsleep 30\n")
	if err := os.Chmod(helper, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_EXEC_PATH", filepath.Dir(helper))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := r.SmudgeLFS(ctx, "a.bin", []byte("pointer"), io.Discard)
	if took := time.Since(start); err == nil || took > 10*time.Second {
		t.Fatalf("SmudgeLFS = %v after %s, want an error once cancelled", err, took)
	}
}

// TestLFSObjectMatchesGitLFS places objects where git-lfs's own storage is.
func TestLFSObjectMatchesGitLFS(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	const oid = "6075562b771f8b82f0c179e363389684f2dd09de30038269e2628e504bd7be0f"
	for _, storage := range []string{"", "custom", "~/literal", filepath.Join(t.TempDir(), "absolute")} {
		t.Run("lfs.storage="+storage, func(t *testing.T) {
			r := testRepo(t)
			if storage != "" {
				testGit(t, r.Top, "config", "lfs.storage", storage)
			}
			var media string
			for line := range strings.Lines(testGit(t, r.Top, "lfs", "env")) {
				if dir, ok := strings.CutPrefix(strings.TrimSpace(line), "LocalMediaDir="); ok {
					media = dir
				}
			}
			got, err := r.LFSObject(oid)
			if err != nil {
				t.Fatal(err)
			}
			// Paths are compared as files: git-lfs may spell them differently
			// (short names, separators on Windows).
			want := filepath.Join(media, oid[:2], oid[2:4], oid)
			if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(want, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			wantInfo, err := os.Stat(want)
			if err != nil {
				t.Fatal(err)
			}
			if gotInfo, err := os.Stat(got); err != nil || !os.SameFile(gotInfo, wantInfo) {
				t.Fatalf("LFSObject = %s, git-lfs keeps it at %s", got, want)
			}
		})
	}
}
