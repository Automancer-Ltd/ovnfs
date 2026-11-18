package checkout

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/store"
)

// TestUnifiedMatchesGNUDiff compares edge cases with `diff -u --label` output
// from GNU diffutils 3.12: missing final newlines, empty and absent sides,
// single-line ranges, and the gap at which neighboring hunks merge. The
// header is what a plain patch writes for a file that keeps its mode.
func TestUnifiedMatchesGNUDiff(t *testing.T) {
	file := func(data []byte) store.File {
		if data == nil {
			return store.File{}
		}
		return store.File{ID: store.OID("id of " + string(data)), Mode: 0o100644}
	}
	for _, test := range []struct {
		name     string
		old, cur []byte
		want     string
	}{
		{"noeol change", []byte("a\nb\nc"), []byte("a\nb\nd"), "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n a\n b\n-c\n\\ No newline at end of file\n+d\n\\ No newline at end of file\n"},
		{"eol added", []byte("a\nb"), []byte("a\nb\n"), "--- a/f\n+++ b/f\n@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+b\n"},
		{"noeol context", []byte("x\na\nb"), []byte("y\na\nb"), "--- a/f\n+++ b/f\n@@ -1,3 +1,3 @@\n-x\n+y\n a\n b\n\\ No newline at end of file\n"},
		{"added", nil, []byte("1\n2\n"), "--- /dev/null\n+++ b/f\n@@ -0,0 +1,2 @@\n+1\n+2\n"},
		{"deleted", []byte("1\n"), nil, "--- a/f\n+++ /dev/null\n@@ -1 +0,0 @@\n-1\n"},
		{"from empty", []byte{}, []byte("1\n"), "--- a/f\n+++ b/f\n@@ -0,0 +1 @@\n+1\n"},
		{"equal", []byte("a\n"), []byte("a\n"), ""},
		{"gap 6 merges", []byte("x\n1\n2\n3\n4\n5\n6\ny\n"), []byte("X\n1\n2\n3\n4\n5\n6\nY\n"), "--- a/f\n+++ b/f\n@@ -1,8 +1,8 @@\n-x\n+X\n 1\n 2\n 3\n 4\n 5\n 6\n-y\n+Y\n"},
		{"gap 7 splits", []byte("x\n1\n2\n3\n4\n5\n6\n7\ny\n"), []byte("X\n1\n2\n3\n4\n5\n6\n7\nY\n"), "--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n-x\n+X\n 1\n 2\n 3\n@@ -6,4 +6,4 @@\n 5\n 6\n 7\n-y\n+Y\n"},
		{"middle", []byte("1\n2\n3\n4\n5\n6\n7\n8\n9\n"), []byte("1\n2\n3\n4\n6\n7\n8\n9\n"), "--- a/f\n+++ b/f\n@@ -2,7 +2,6 @@\n 2\n 3\n 4\n-5\n 6\n 7\n 8\n"},
	} {
		var w strings.Builder
		gitPatch(&w, "f", file(test.old), file(test.cur), test.old, test.cur, false)
		_, got, _ := strings.Cut(w.String(), "\n") // past the diff --git line
		for _, mode := range []string{"new file mode 100644\n", "deleted file mode 100644\n"} {
			got = strings.TrimPrefix(got, mode)
		}
		if got != test.want {
			t.Errorf("%s:\n%s\nwant:\n%s", test.name, got, test.want)
		}
	}
}

// TestDiffOfPaths narrows a diff to files and folders: the changes outside
// them are only counted, Stat counts the lines of those inside and marks the
// binary one, the plain patch of the text ones applies with git apply, and a
// path outside the root is refused.
func TestDiffOfPaths(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n", "src/keep.go": "package src\n", "blob.bin": "\x00old", "other.txt": "o\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "core.autocrlf", "false")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	f.newCheckout(dir, "app")
	f.put("app", "a.txt", "one\n2\nthree\n")
	f.put("app", "src/deep/new.go", "package deep\n\nvar X = 1\n")
	f.put("app", "blob.bin", "\x00new")
	f.put("app", "other.txt", "changed\n")
	f.remove("app", "gone.txt")
	ctx := context.Background()

	stat, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "app", Stat: true, Paths: []string{"./a.txt", "src/", "blob.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.DiffStat{{Path: "a.txt", Added: 2, Deleted: 1}, {Path: "blob.bin", Binary: true}, {Path: "src/deep/new.go", Added: 3}}
	if !slices.Equal(stat.Stat, want) || stat.Left != 2 || len(stat.Changes) != 3 || stat.Patch != "" {
		t.Fatalf("stat of three paths = %+v, left %d, changes %v, patch %q; want %+v with 2 left", stat.Stat, stat.Left, stat.Changes, stat.Patch, want)
	}

	text, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "app", Patch: true, Paths: []string{"a.txt", "src", "gone.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(t.TempDir(), "app.patch")
	if err := os.WriteFile(patch, []byte(text.Patch), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "apply", patch)
	if got := gitRun(t, dir, "status", "--porcelain"); got != "M a.txt\n D gone.txt\n?? src/deep/" { // gitRun trims
		t.Errorf("folder after git apply of the patch:\n%s\npatch:\n%s", got, text.Patch)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "src/deep/new.go")); err != nil || string(got) != "package deep\n\nvar X = 1\n" {
		t.Errorf("src/deep/new.go = %q, %v", got, err)
	}

	if all, err := f.reg.Diff(ctx, api.DiffArgs{Checkout: "app", Paths: []string{"nowhere", "."}}); err != nil || len(all.Changes) != 5 || all.Left != 0 {
		t.Errorf("the root among the paths = %+v, %v; want every change", all, err)
	}
	_, err = f.reg.Diff(ctx, api.DiffArgs{Checkout: "app", Paths: []string{"src/../../etc"}})
	if coded, ok := errors.AsType[*Error](err); !ok || coded.Code != "invalid" {
		t.Errorf("a path above the root: %v, want invalid", err)
	}
}

// TestBinaryPatchAppliesWithGit applies a checkout's --binary patch to the folder
// it came from with git apply and finds the folder then holding what the checkout
// does: edited, added and deleted text, binary files added and changed, and names
// git has to quote.
func TestBinaryPatchAppliesWithGit(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"a.txt": "one\ntwo\n", "gone.txt": "bye\n", "blob.bin": "\x00\x01old"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "core.autocrlf", "false")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	f.newCheckout(dir, "app")
	want := map[string]string{
		"a.txt":          "one\n2\nthree",
		"blob.bin":       "\x00\x01new\xff",
		"new.bin":        strings.Repeat("\x00\x7f\xfe", 300),
		"spaced name.md": "x\n",
	}
	if runtime.GOOS != "windows" { // no double quote in Windows names
		want[`quo"te.txt`] = "q\n"
	}
	for p, data := range want {
		f.put("app", p, data)
	}
	f.remove("app", "gone.txt")
	reply, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "app", Binary: true})
	if err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(t.TempDir(), "app.patch")
	if err := os.WriteFile(patch, []byte(reply.Patch), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "apply", "--check", patch)
	gitRun(t, dir, "apply", patch)
	for p, data := range want {
		got, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || string(got) != data {
			t.Errorf("%s after git apply = %q, %v; want %q\npatch:\n%s", p, got, err, data, reply.Patch)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt survived git apply: %v", err)
	}
}

// TestBinaryPatchStoresLineEndingsAsGit checks that with core.autocrlf a git patch
// holds what git stores: CRLF work-tree text as LF with the index's blob ids, so
// it applies to the index; a file whose index blob keeps CRLF stays as it is; and
// a change of line endings git normalizes away is none.
func TestBinaryPatchStoresLineEndingsAsGit(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{"kept.txt": "a\r\nb\r\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "core.autocrlf", "false")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "crlf blob")
	gitRun(t, dir, "config", "core.autocrlf", "true")
	for p, data := range map[string]string{"crlf.txt": "one\r\ntwo\r\n", "eol.txt": "x\r\n", "data.bin": "\x00\r\n"} {
		write(t, filepath.Join(dir, p), data)
	}
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "lf blobs")
	f.newCheckout(dir, "app")
	f.put("app", "crlf.txt", "one\r\n2\r\n")
	f.put("app", "kept.txt", "a\r\nB\r\n")
	f.put("app", "eol.txt", "x\n")
	f.put("app", "data.bin", "\x00\r\n!")
	reply, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "app", Binary: true})
	if err != nil {
		t.Fatal(err)
	}
	var changed []string
	for _, c := range reply.Changes {
		changed = append(changed, c.Path)
	}
	if want := []string{"crlf.txt", "data.bin", "kept.txt"}; !slices.Equal(changed, want) {
		t.Fatalf("changes %v, want %v", changed, want)
	}
	for _, want := range []string{
		"index " + gitRun(t, dir, "rev-parse", ":crlf.txt") + "..",
		"-two\n+2\n",
		"-b\r\n+B\r\n",
	} {
		if !strings.Contains(reply.Patch, want) {
			t.Errorf("patch lacks %q:\n%s", want, reply.Patch)
		}
	}
	patch := filepath.Join(t.TempDir(), "app.patch")
	if err := os.WriteFile(patch, []byte(reply.Patch), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "apply", "--cached", "--check", patch)
	gitRun(t, dir, "apply", "--check", patch)
}

// TestBinaryPatchRefusesFilteredPaths refuses a git patch of a change git would
// store through a filter driver: only git can tell what it stores.
func TestBinaryPatchRefusesFilteredPaths(t *testing.T) {
	f := newFixture(t)
	dir := folder(t, map[string]string{".gitattributes": "*.big filter=big\n", "a.big": "x\n"})
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "filter.big.clean", "cat")
	gitRun(t, dir, "add", "--all")
	gitRun(t, dir, "commit", "-qm", "init")
	f.newCheckout(dir, "app")
	f.put("app", "a.big", "y\n")
	_, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "app", Binary: true})
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != "unsupported" {
		t.Fatalf("diff of a filtered path: %v, want code unsupported", err)
	}
	if _, err := f.reg.Diff(context.Background(), api.DiffArgs{Checkout: "app", Patch: true}); err != nil {
		t.Fatalf("plain patch of a filtered path: %v", err)
	}
}
