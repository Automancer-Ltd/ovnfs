package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
)

// changed lists a reply's applied changes as "STATUS path".
func changed(changes []api.Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = c.Status + " " + c.Path
	}
	return out
}

// TestPullFolderMergesWhatTheFolderChanged checks a pull from the folder, also
// across a restart: paths only the folder changed arrive, text both changed is
// merged, overlapping lines get conflict markers in the checkout and stay out
// of the folder until resolved, and from then on diff and apply see only what
// the checkout changed beyond the folder it pulled.
func TestPullFolderMergesWhatTheFolderChanged(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{
		"merged.txt": "1\n2\n3\n", "theirs.txt": "old\n", "gone.txt": "x\n", "both.txt": "one\ntwo\nthree\n", "mine.txt": "m\n",
	})
	f.newCheckout(dir, "c")
	f.put("c", "merged.txt", "ONE\n2\n3\n")
	f.put("c", "both.txt", "one\nmine\nthree\n")
	f.put("c", "mine.txt", "M\n")
	// The user keeps working in the folder meanwhile.
	write(t, filepath.Join(dir, "merged.txt"), "1\n2\nTHREE\n")
	write(t, filepath.Join(dir, "theirs.txt"), "new\n")
	write(t, filepath.Join(dir, "added.txt"), "added\n")
	write(t, filepath.Join(dir, "both.txt"), "one\nfolder\nthree\n")
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	dry, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: "c", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.read("c", "theirs.txt"); got != "old\n" {
		t.Fatalf("a dry run wrote theirs.txt: %q", got)
	}
	pull, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed(dry.Applied), changed(pull.Applied)) || !slices.Equal(dry.Merged, pull.Merged) || len(dry.Conflicts) != len(pull.Conflicts) {
		t.Errorf("dry run = %+v, pull = %+v", dry, pull)
	}
	if got, want := changed(pull.Applied), []string{"A added.txt", "D gone.txt", "M theirs.txt"}; !slices.Equal(got, want) {
		t.Errorf("applied = %v, want %v", got, want)
	}
	if !slices.Equal(pull.Merged, []string{"merged.txt"}) || len(pull.Conflicts) != 1 || pull.Conflicts[0].Path != "both.txt" || !pull.Conflicts[0].Markers {
		t.Errorf("merged = %v, conflicts = %+v", pull.Merged, pull.Conflicts)
	}
	markers := "one\n<<<<<<< checkout c\nmine\n=======\nfolder\n>>>>>>> folder\nthree\n"
	for p, want := range map[string]string{"merged.txt": "ONE\n2\nTHREE\n", "theirs.txt": "new\n", "added.txt": "added\n", "gone.txt": "<absent>", "both.txt": markers, "mine.txt": "M\n"} {
		if got := f.read("c", p); got != want {
			t.Errorf("checkout %s = %q, want %q", p, got, want)
		}
	}
	if !f.invalidatedPath("c", "theirs.txt") {
		t.Error("the pull did not invalidate what it wrote")
	}

	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := changed(diff.Changes), []string{"M both.txt", "M merged.txt", "M mine.txt"}; !slices.Equal(got, want) {
		t.Errorf("diff after the pull = %v, want only the checkout's own %v", got, want)
	}
	held, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := changed(held.Applied), []string{"M merged.txt", "M mine.txt"}; !slices.Equal(got, want) || len(held.Conflicts) != 1 || held.Conflicts[0].Path != "both.txt" {
		t.Errorf("apply with markers in the checkout = %+v", held)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "both.txt")); string(data) != "one\nfolder\nthree\n" {
		t.Errorf("unresolved markers reached the folder: %q", data)
	}
	again, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: "c"})
	if err != nil || len(again.Applied)+len(again.Merged) != 0 || len(again.Conflicts) != 1 {
		t.Errorf("pull with markers pending = %+v, %v", again, err)
	}

	f.put("c", "both.txt", "one\nmine and folder\nthree\n")
	resolved, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c"})
	if err != nil || !slices.Equal(changed(resolved.Applied), []string{"M both.txt"}) || len(resolved.Conflicts) != 0 {
		t.Fatalf("apply after resolving = %+v, %v", resolved, err)
	}
	for p, want := range map[string]string{"merged.txt": "ONE\n2\nTHREE\n", "both.txt": "one\nmine and folder\nthree\n", "mine.txt": "M\n", "theirs.txt": "new\n"} {
		if data, err := os.ReadFile(filepath.Join(dir, p)); err != nil || string(data) != want {
			t.Errorf("folder %s = %q, %v; want %q", p, data, err, want)
		}
	}
	if settled, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: "c"}); err != nil || len(settled.Applied)+len(settled.Merged)+len(settled.Conflicts) != 0 {
		t.Errorf("pull once both agree = %+v, %v", settled, err)
	}
}

// TestPullFolderAfterApply checks that a pull takes what an apply left in the
// folder as already shared: the folder's later edit arrives without the
// applied lines merging into the checkout a second time, and the line the
// checkout then edits again applies cleanly.
func TestPullFolderAfterApply(t *testing.T) {
	isolateGit(t)
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "one\ntwo\nthree\n"})
	f.newCheckout(dir, "c")
	f.put("c", "a.txt", "one\nTWO\nthree\n")
	ctx := context.Background()
	if _, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c"}); err != nil {
		t.Fatal(err)
	}
	// The user edits the applied line itself.
	write(t, filepath.Join(dir, "a.txt"), "one\nTWO, reviewed\nthree\n")
	pull, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: "c"})
	if err != nil || !slices.Equal(changed(pull.Applied), []string{"M a.txt"}) || len(pull.Conflicts) != 0 {
		t.Fatalf("pull = %+v, %v", pull, err)
	}
	if diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "c"}); err != nil || len(diff.Changes) != 0 {
		t.Errorf("diff once the checkout holds the folder's file = %+v, %v", diff.Changes, err)
	}
	// A fork holds the same files, so it counts from the same agreement.
	if _, err := f.reg.Fork(ctx, api.ForkArgs{Checkout: "c", Name: "fork"}); err != nil {
		t.Fatal(err)
	}
	f.put("fork", "a.txt", "one\nTWO, reviewed\nthree\nfour\n")
	if diff, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "fork", Patch: true}); err != nil || strings.Contains(diff.Patch, "-two") || !strings.Contains(diff.Patch, "+four") {
		t.Errorf("fork's diff counts from the capture, not from what its parent pulled: %v\n%s", err, diff.Patch)
	}
	f.put("c", "a.txt", "one\nTWO, reviewed twice\nthree\n")
	apply, err := f.reg.Apply(ctx, api.ApplyArgs{Checkout: "c"})
	if err != nil || !slices.Equal(changed(apply.Applied), []string{"M a.txt"}) || len(apply.Conflicts) != 0 {
		t.Fatalf("apply after the pull = %+v, %v", apply, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(data) != "one\nTWO, reviewed twice\nthree\n" {
		t.Errorf("folder = %q", data)
	}
}

// TestPullFolderRefusesAChosenStart checks that a checkout opened on a revision
// refuses a pull, which would bring in the uncommitted edits --rev left out,
// and that the refusal survives a restart.
func TestPullFolderRefusesAChosenStart(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	write(t, filepath.Join(dir, "tracked.txt"), "uncommitted\n")
	if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: "clean", Rev: "HEAD"}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.Close(); err != nil {
		t.Fatal(err)
	}
	f.open()
	_, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: "clean"})
	if coded, ok := errors.AsType[*Error](err); !ok || coded.Code != "invalid" || !strings.Contains(coded.Message, "git rebase") {
		t.Fatalf("pull into a --rev checkout = %v, want an invalid error naming the rebase", err)
	}
	if got := f.read("clean", "tracked.txt"); got != "original\n" {
		t.Errorf("the refused pull changed tracked.txt: %q", got)
	}
}

// TestPullFolderSaysWhatItLeavesToGit checks the warning of a pull that
// merged files while the folder's HEAD had moved: how to follow it for a
// checkout without commits of its own, how to rebase one that has them, and
// nothing while the folder's HEAD is where the checkout started.
func TestPullFolderSaysWhatItLeavesToGit(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	f := newFixture(t)
	dir := revisionRepo(t)
	for _, name := range []string{"follows", "diverged"} {
		if _, err := f.reg.New(ctx, api.NewArgs{Dir: dir, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if still, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: "follows"}); err != nil || len(still.Warnings) != 0 {
		t.Fatalf("pull before the folder moved = %+v, %v", still, err)
	}
	gitdir := f.gitdir("diverged")
	gitRun(t, dir, "--git-dir", gitdir, "--work-tree", dir, "commit", "-qm", "the checkout's own", "--allow-empty")
	gitRun(t, dir, "commit", "-qm", "the folder moved", "--allow-empty")
	for name, want := range map[string]string{"follows": "git reset FETCH_HEAD", "diverged": "git rebase --autostash FETCH_HEAD"} {
		pull, err := f.reg.PullFolder(ctx, api.PullArgs{Checkout: name})
		if err != nil {
			t.Fatal(err)
		}
		if len(pull.Warnings) != 1 || !strings.Contains(pull.Warnings[0], want) || !strings.Contains(pull.Warnings[0], dir) {
			t.Errorf("pull %s warned %q, want one naming %s and %q", name, pull.Warnings, dir, want)
		}
	}
}
