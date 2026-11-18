package git

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchBranchIntoPartialClone checks a checkout whose commits build on
// history it fetched itself, in a repository cloned without blobs: upstream
// changed one file twice, so the checkout holds the newer version, which its
// merge needed, and nobody holds the older one. While the folder's repository
// lacks that history the commits are refused with what to fetch there; once it
// has it, they arrive, on another branch and on the checked-out one, though
// git fetch would refuse them for naming a blob the repository was only
// promised, and the repository stays sound for git.
func TestFetchBranchIntoPartialClone(t *testing.T) {
	upstream := testRepo(t)
	testWrite(t, filepath.Join(upstream.Top, "twice.txt"), "v1\n")
	testGit(t, upstream.Top, "add", "--all")
	testGit(t, upstream.Top, "commit", "-qm", "base")
	testGit(t, upstream.Top, "config", "uploadpack.allowFilter", "true")
	top := filepath.Join(t.TempDir(), "folder")
	testGit(t, upstream.Top, "clone", "-q", "--filter=blob:none", "file://"+upstream.Top, top)
	testGit(t, top, "config", "user.email", "test@example.com")
	testGit(t, top, "config", "user.name", "test")
	r, err := Open(top)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v2\n", "v3\n"} {
		testWrite(t, filepath.Join(upstream.Top, "twice.txt"), version)
		testGit(t, upstream.Top, "commit", "-qam", "upstream "+strings.TrimSpace(version))
	}
	gitdir, work := branchCheckout(t, r)
	testWrite(t, filepath.Join(work, "tracked"), "agent\n")
	testGit(t, work, "commit", "-qam", "agent")
	testGit(t, work, "fetch", "-q", "origin")
	testGit(t, work, "merge", "-q", "--no-edit", "origin/main")
	want := testGit(t, work, "rev-parse", "HEAD")
	ctx := context.Background()

	_, _, err = r.FetchBranch(ctx, gitdir, "topic", false)
	partial, ok := errors.AsType[*PartialCloneError](err)
	if !ok || len(partial.Refs) != 1 || partial.Refs[0] != "origin/main" || !strings.Contains(err.Error(), "fetch origin") {
		t.Fatalf("fetch before the folder has upstream's history = %v, want a PartialCloneError naming origin/main", err)
	}
	if out := testGit(t, top, "branch", "--list", "topic"); out != "" {
		t.Fatalf("the refused fetch created %q", out)
	}

	testGit(t, top, "fetch", "-q", "origin")
	if commit, _, err := r.FetchBranch(ctx, gitdir, "topic", false); err != nil || commit != want {
		t.Fatalf("fetch as another branch = %s, %v; want %s", commit, err, want)
	}
	if commit, _, err := r.FetchBranch(ctx, gitdir, "main", false); err != nil || commit != want {
		t.Fatalf("fetch as the checked-out branch = %s, %v; want %s", commit, err, want)
	}
	for _, ref := range []string{"topic", "HEAD"} {
		if got := testGit(t, top, "rev-parse", ref); got != want {
			t.Errorf("%s = %s, want %s", ref, got, want)
		}
	}
	// Neither may reach for the promisor remote: what is missing was promised.
	t.Setenv("GIT_NO_LAZY_FETCH", "1")
	testGit(t, top, "fsck", "--connectivity-only")
	testGit(t, top, "gc", "-q")
}
