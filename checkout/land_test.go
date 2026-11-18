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

// landFixture is a repository on main with a committed three-file tree and a
// checkout "work" opened on its HEAD.
type landFixture struct {
	*fixture
	dir, old string
	work     string // a work tree for git commands against the checkout's private git directory
}

func newLandFixture(t *testing.T, rev string) *landFixture {
	t.Helper()
	isolateGit(t)
	f := &landFixture{fixture: newFixture(t), work: t.TempDir()}
	f.dir = folder(t, map[string]string{"a.txt": "one\ntwo\nthree\n", "b.txt": "b\n", "c.txt": "c\n"})
	gitRun(t, f.dir, "init", "-q", "-b", "main")
	gitRun(t, f.dir, "add", "--all")
	gitRun(t, f.dir, "commit", "-qm", "initial")
	f.old = gitRun(t, f.dir, "rev-parse", "HEAD")
	if _, err := f.reg.New(context.Background(), api.NewArgs{Dir: f.dir, Name: "work", Rev: rev}); err != nil {
		t.Fatal(err)
	}
	return f
}

// commit writes files in the checkout and commits exactly them there.
func (f *landFixture) commit(message string, files map[string]string) string {
	f.t.Helper()
	git := func(args ...string) string {
		return gitRun(f.t, f.work, append([]string{"--git-dir", f.gitdir("work"), "--work-tree", f.work}, args...)...)
	}
	for p, data := range files {
		f.put("work", p, data)
		write(f.t, filepath.Join(f.work, p), data)
		git("add", p)
	}
	git("commit", "-qm", message)
	return git("rev-parse", "HEAD")
}

// folderFile reads a file of the folder.
func (f *landFixture) folderFile(p string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, p))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// TestLandKeepsTheFoldersEdits lands a checkout's commits on the folder's
// checked-out branch while the folder holds uncommitted work: the branch and
// the files move together, the user's edits stay on top as the only changes
// git reports, what the checkout did not commit stays there, and landing again
// has nothing left to do.
func TestLandKeepsTheFoldersEdits(t *testing.T) {
	f := newLandFixture(t, "HEAD")
	ctx := context.Background()
	// The user's uncommitted work: another line of a file the commits change,
	// a file they do not touch, and a staged one.
	write(t, filepath.Join(f.dir, "a.txt"), "one\ntwo\nthree, the user's\n")
	write(t, filepath.Join(f.dir, "b.txt"), "the user's b\n")
	write(t, filepath.Join(f.dir, "staged.txt"), "staged\n")
	gitRun(t, f.dir, "add", "staged.txt")
	f.commit("first", map[string]string{"a.txt": "ONE\ntwo\nthree\n"})
	want := f.commit("second", map[string]string{"c.txt": "C\n", "new.txt": "new\n"})
	f.put("work", "wip.txt", "not committed\n")

	dry, err := f.reg.Land(ctx, api.LandArgs{Checkout: "work", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitRun(t, f.dir, "rev-parse", "HEAD"); got != f.old || f.folderFile("c.txt") != "c\n" {
		t.Fatalf("a dry run moved HEAD to %s or wrote c.txt", got)
	}
	land, err := f.reg.Land(ctx, api.LandArgs{Checkout: "work"})
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]api.LandReply{"dry run": dry, "land": land} {
		if r.Branch != "main" || r.Old != f.old || r.Commit != want || r.Left != 1 || r.Refused ||
			!slices.Equal(changed(r.Applied), []string{"M c.txt", "A new.txt"}) || !slices.Equal(r.Merged, []string{"a.txt"}) || len(r.Conflicts) != 0 {
			t.Errorf("%s = %+v", name, r)
		}
	}
	if got := gitRun(t, f.dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("HEAD = %s, want %s", got, want)
	}
	if got := gitRun(t, f.dir, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Errorf("the folder is on %q, want main", got)
	}
	// Only the user's own work is left for git to report (gitRun trims the
	// first line's leading space).
	if got, want := gitRun(t, f.dir, "status", "--porcelain"), "M a.txt\n M b.txt\nA  staged.txt"; got != want {
		t.Errorf("status = %q, want the user's edits only: %q", got, want)
	}
	for p, want := range map[string]string{"a.txt": "ONE\ntwo\nthree, the user's\n", "b.txt": "the user's b\n", "c.txt": "C\n", "new.txt": "new\n"} {
		if got := f.folderFile(p); got != want {
			t.Errorf("folder %s = %q, want %q", p, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, "wip.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the checkout's uncommitted wip.txt reached the folder: %v", err)
	}
	gitRun(t, f.dir, "fsck", "--strict")

	again, err := f.reg.Land(ctx, api.LandArgs{Checkout: "work"})
	if err != nil || again.Old != want || again.Commit != want || len(again.Applied)+len(again.Merged) != 0 {
		t.Errorf("landing again = %+v, %v; want nothing to land", again, err)
	}
}

// TestLandRefusesBeforeWriting checks that whatever stands in the way stops a
// land before the folder or its repository changes: an overlapping uncommitted
// edit, a branch that moved, staged changes on the commits' paths, and a
// detached HEAD.
func TestLandRefusesBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(f *landFixture)
		refused bool   // the reply is Refused with a conflict on a.txt
		message string // or the error is a conflict saying this
	}{
		{name: "overlapping edit", refused: true, prepare: func(f *landFixture) {
			write(f.t, filepath.Join(f.dir, "a.txt"), "the user's\ntwo\nthree\n")
		}},
		{name: "branch moved", message: "git rebase --autostash FETCH_HEAD", prepare: func(f *landFixture) {
			gitRun(f.t, f.dir, "commit", "-qm", "the user's", "--allow-empty")
		}},
		{name: "staged", message: "staged changes in a.txt", prepare: func(f *landFixture) {
			write(f.t, filepath.Join(f.dir, "a.txt"), "staged\n")
			gitRun(f.t, f.dir, "add", "a.txt")
		}},
		{name: "detached", message: "apply work --branch", prepare: func(f *landFixture) {
			gitRun(f.t, f.dir, "checkout", "-q", "--detach")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLandFixture(t, "HEAD")
			f.commit("change", map[string]string{"a.txt": "ONE\ntwo\nthree\n", "b.txt": "B\n"})
			tc.prepare(f)
			head, status, a := gitRun(t, f.dir, "rev-parse", "HEAD"), gitRun(t, f.dir, "status", "--porcelain"), f.folderFile("a.txt")
			reply, err := f.reg.Land(context.Background(), api.LandArgs{Checkout: "work"})
			if tc.refused {
				if err != nil || !reply.Refused || len(reply.Conflicts) != 1 || reply.Conflicts[0].Path != "a.txt" || reply.Commit != reply.Old {
					t.Fatalf("land = %+v, %v; want it refused over a.txt", reply, err)
				}
			} else if coded, ok := errors.AsType[*Error](err); !ok || coded.Code != "conflict" || !strings.Contains(coded.Message, tc.message) {
				t.Fatalf("land = %+v, %v; want a conflict saying %q", reply, err, tc.message)
			}
			if got := gitRun(t, f.dir, "rev-parse", "HEAD"); got != head {
				t.Errorf("HEAD moved to %s", got)
			}
			if got := gitRun(t, f.dir, "status", "--porcelain"); got != status {
				t.Errorf("status = %q, was %q", got, status)
			}
			if got := f.folderFile("a.txt"); got != a || f.folderFile("b.txt") != "b\n" {
				t.Errorf("the folder's files changed: a.txt = %q", got)
			}
		})
	}
}

// TestLandWithMarkersResolvesInTheFolder checks the way out of a refused land
// for a checkout that cannot pull: with Markers the folder gets the conflict
// markers and the other files while the branch stays, and once the markers are
// resolved a land moves the branch, leaving the resolution as the user's
// uncommitted edit.
func TestLandWithMarkersResolvesInTheFolder(t *testing.T) {
	f := newLandFixture(t, "HEAD")
	ctx := context.Background()
	want := f.commit("change", map[string]string{"a.txt": "ONE\ntwo\nthree\n", "b.txt": "B\n"})
	write(t, filepath.Join(f.dir, "a.txt"), "the user's\ntwo\nthree\n")

	refused, err := f.reg.Land(ctx, api.LandArgs{Checkout: "work"})
	if err != nil || !refused.Refused || refused.Pullable {
		t.Fatalf("land = %+v, %v; want it refused, and no pull offered to a checkout opened on a revision", refused, err)
	}
	marked, err := f.reg.Land(ctx, api.LandArgs{Checkout: "work", Markers: true})
	if err != nil || marked.Refused || len(marked.Conflicts) != 1 || !marked.Conflicts[0].Markers || marked.Commit != f.old {
		t.Fatalf("land with markers = %+v, %v", marked, err)
	}
	if got := gitRun(t, f.dir, "rev-parse", "HEAD"); got != f.old {
		t.Fatalf("HEAD moved to %s while a.txt holds conflict markers", got)
	}
	if a := f.folderFile("a.txt"); !strings.Contains(a, "<<<<<<< folder\nthe user's\n") || !strings.Contains(a, ">>>>>>> checkout work\n") || f.folderFile("b.txt") != "B\n" {
		t.Fatalf("folder after land with markers: a.txt = %q, b.txt = %q", a, f.folderFile("b.txt"))
	}
	if again, err := f.reg.Land(ctx, api.LandArgs{Checkout: "work"}); err != nil || !again.Refused {
		t.Fatalf("land over unresolved markers = %+v, %v; want it refused", again, err)
	}

	write(t, filepath.Join(f.dir, "a.txt"), "ONE, the user's\ntwo\nthree\n")
	landed, err := f.reg.Land(ctx, api.LandArgs{Checkout: "work"})
	if err != nil || landed.Commit != want || len(landed.Conflicts) != 0 {
		t.Fatalf("land after resolving = %+v, %v", landed, err)
	}
	if got := gitRun(t, f.dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("HEAD = %s, want %s", got, want)
	}
	if got := gitRun(t, f.dir, "status", "--porcelain"); got != "M a.txt" || f.folderFile("a.txt") != "ONE, the user's\ntwo\nthree\n" {
		t.Errorf("status = %q, a.txt = %q; want the resolution as the only uncommitted edit", got, f.folderFile("a.txt"))
	}
}

// TestLandCommitsWhatTheFolderHolds lands commits made in a checkout of a
// dirty folder that pick some of its uncommitted work: the folder's files are
// what was committed already, so only the branch moves, and git reports what
// was left uncommitted.
func TestLandCommitsWhatTheFolderHolds(t *testing.T) {
	isolateGit(t)
	f := &landFixture{fixture: newFixture(t), work: t.TempDir()}
	f.dir = folder(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	gitRun(t, f.dir, "init", "-q", "-b", "main")
	gitRun(t, f.dir, "add", "--all")
	gitRun(t, f.dir, "commit", "-qm", "initial")
	write(t, filepath.Join(f.dir, "a.txt"), "a, edited\n")
	write(t, filepath.Join(f.dir, "b.txt"), "b, edited\n")
	f.newCheckout(f.dir, "work")
	// The checkout holds both edits; only a.txt's is committed there.
	want := f.commit("picked a", map[string]string{"a.txt": "a, edited\n"})

	land, err := f.reg.Land(context.Background(), api.LandArgs{Checkout: "work"})
	if err != nil || land.Commit != want || len(land.Applied)+len(land.Merged)+len(land.Conflicts) != 0 {
		t.Fatalf("land = %+v, %v; want only the branch to move", land, err)
	}
	if got := gitRun(t, f.dir, "status", "--porcelain"); got != "M b.txt" {
		t.Errorf("status = %q, want only b.txt left uncommitted", got)
	}
	if f.folderFile("a.txt") != "a, edited\n" || f.folderFile("b.txt") != "b, edited\n" {
		t.Errorf("the folder's files changed: %q %q", f.folderFile("a.txt"), f.folderFile("b.txt"))
	}
}
