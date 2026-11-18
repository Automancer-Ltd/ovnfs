package git

import (
	"context"
	"path/filepath"
	"testing"
)

// TestUnshared walks a checkout's commit through the states a removal may find
// it in: only the checkout's, copied into the repository with no ref on it,
// named by the tracking ref that goes with the checkout, and then kept by a
// branch, by a descendant made in the repository, and by a push from the
// checkout. A commit the repository amended away is borrowed, never counted.
func TestUnshared(t *testing.T) {
	r := testRepo(t)
	ctx := context.Background()
	gitdir, work := branchCheckout(t, r)
	held := func(when string, want int) Unshared {
		t.Helper()
		got, err := r.Unshared(ctx, "work", gitdir)
		if err != nil || got.Commits != want {
			t.Fatalf("%s: unshared = %+v, %v; want %d", when, got, err, want)
		}
		return got
	}

	held("a fresh checkout", 0)
	testGit(t, r.Top, "commit", "-q", "--amend", "-m", "amended")
	held("the repository amended the commit the checkout's branch names", 0)

	testWrite(t, filepath.Join(work, "made"), "here\n")
	testGit(t, work, "add", "made")
	testGit(t, work, "commit", "-qm", "made here")
	testGit(t, work, "checkout", "-q", "--detach")
	testWrite(t, filepath.Join(work, "made"), "here, twice\n")
	testGit(t, work, "commit", "-qam", "made on a detached HEAD")
	tip := testGit(t, work, "rev-parse", "HEAD")
	if got := held("two commits made in the checkout", 2); got.Tip != tip || got.Subject != "made on a detached HEAD" {
		t.Fatalf("tip = %s %q, want HEAD %s", got.Tip, got.Subject, tip)
	}

	if err := r.FetchCommits(ctx, gitdir); err != nil {
		t.Fatal(err)
	}
	held("copied into the repository, on no ref", 2)
	testGit(t, r.Top, "update-ref", "refs/remotes/ovnfs/work/HEAD", tip)
	held("named by the tracking ref that goes with the checkout", 2)

	testGit(t, r.Top, "update-ref", "refs/heads/kept", tip+"~1")
	held("a branch on the first of them", 1)
	on := testGit(t, r.Top, "commit-tree", "-p", tip, "-m", "on top", tip+"^{tree}")
	testGit(t, r.Top, "update-ref", "refs/heads/kept", on)
	held("a branch on a descendant of both", 0)

	testGit(t, r.Top, "update-ref", "-d", "refs/heads/kept")
	held("the branch deleted again", 2)
	testGit(t, work, "update-ref", "refs/remotes/origin/pushed", tip)
	held("pushed from the checkout", 0)
}
