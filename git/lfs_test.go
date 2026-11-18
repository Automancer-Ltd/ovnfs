package git

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		{"legacy version", "version https://hawser.github.com/spec/v1\noid sha256:" + oid + "\nsize 0\n", LFSPointer{OID: oid}, true},
		{"no final newline", version + "oid sha256:" + oid + "\nsize 1", LFSPointer{}, false},
		{"extension", version + "ext-0-foo sha256:" + oid + "\noid sha256:" + oid + "\nsize 1\n", LFSPointer{}, false},
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

// TestLFSCheckout follows git: filter.lfs.process wins over filter.lfs.smudge,
// and git-lfs leaves pointers for --skip and GIT_LFS_SKIP_SMUDGE.
func TestLFSCheckout(t *testing.T) {
	for _, tc := range []struct {
		name, process, smudge, env string
		want                       bool
	}{
		{name: "no driver"},
		{name: "smudge", smudge: "git-lfs smudge -- %f", want: true},
		{name: "process", process: "git-lfs filter-process", want: true},
		{name: "process skips", process: "git-lfs filter-process --skip", smudge: "git-lfs smudge -- %f"},
		{name: "process wins", process: "git-lfs filter-process", smudge: "git-lfs smudge --skip -- %f", want: true},
		{name: "env skips", process: "git-lfs filter-process", env: "1"},
		{name: "env off", process: "git-lfs filter-process", env: "0", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRepo(t)
			t.Setenv("GIT_LFS_SKIP_SMUDGE", tc.env)
			if tc.process != "" {
				testGit(t, r.Top, "config", "filter.lfs.process", tc.process)
			}
			if tc.smudge != "" {
				testGit(t, r.Top, "config", "filter.lfs.smudge", tc.smudge)
			}
			got, err := r.LFSCheckout()
			if err != nil || got != tc.want {
				t.Fatalf("LFSCheckout = %v, %v; want %v", got, err, tc.want)
			}
		})
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
			if want := filepath.Join(media, oid[:2], oid[2:4], oid); got != want {
				t.Fatalf("LFSObject = %s, want %s", got, want)
			}
		})
	}
}
