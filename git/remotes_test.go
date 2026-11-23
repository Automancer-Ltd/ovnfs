package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestIncludeConfig adds and removes the remotes include as git reads the config:
// afterwards git lists the include exactly once, or not at all, and every other
// variable is as it was.
func TestIncludeConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	list := func(t *testing.T, config string) (rest []string, paths int) {
		t.Helper()
		file := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(file, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("git", "config", "--file", file, "--list").CombinedOutput()
		if err != nil {
			t.Fatalf("git config --list: %v\n%s\n%q", err, out, config)
		}
		for line := range strings.Lines(strings.TrimSpace(string(out))) {
			line = strings.TrimSuffix(line, "\n")
			if line == "include.path=X" {
				paths++
			} else {
				rest = append(rest, line)
			}
		}
		return rest, paths
	}
	cases := map[string]string{
		"comment ending in backslash": "[core]\n\teditor = vim ; note \\\n[include]\n\tpath = X\n",
		"kept continuation":           "[include]\n\tother = a\\\n\tpath = X\n[core]\n\tbare = false\n",
		"quoted continuation":         "[core]\n\teditor = \"vim \\\n; #\"\n[include]\n\tpath = \"X\"\n\tother = kept\n",
		"quoted bracket in header":    "[include \"a]b\"]\n\tpath = X\n[core]\n\tbare = false\n",
		"header value continued":      "[core]\n\tbare = false\n[alias] st = status \\\n[include]\n\tpath = X\n",
		"continued to end of file":    "[alias]\n\tst = status --short\\\n",
		"inline comment on header":    "[include] # note\n\tpath = X\n[core]\n\tbare = false\n",
		"inline path on header":       "[include] path = X\n[core]\n\tbare = false\n",
		"byte order mark":             "\xef\xbb\xbf[include]\n\tpath = X\n[core]\n\tbare = false\n",
		"two headers on a line":       "[core][include] path = X\n",
		"CRLF":                        "[core]\r\n\tbare = false\r\n[include]\r\n\tpath = X\r\n",
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			before, _ := list(t, config)
			added := includeConfig(config, "X", true)
			removed := includeConfig(added, "X", false)
			for what, c := range map[string]struct {
				config string
				paths  int
			}{"added": {added, 1}, "removed": {removed, 0}} {
				rest, paths := list(t, c.config)
				if !slices.Equal(rest, before) {
					t.Fatalf("%s changed other variables:\n%q\nfrom %q", what, c.config, config)
				}
				if paths != c.paths {
					t.Fatalf("%s: git reads include.path %d times, want %d:\n%q", what, paths, c.paths, c.config)
				}
			}
		})
	}
	t.Run("git sees the include", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "config")
		if err := os.WriteFile(filepath.Join(dir, "X"), []byte("[ovnfs]\n\tfromX = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		count := func(config string) int {
			t.Helper()
			if err := os.WriteFile(file, []byte(config), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("git", "config", "--file", file, "--includes", "--get-all", "ovnfs.fromx").Output()
			if ee, ok := err.(*exec.ExitError); err != nil && !(ok && ee.ExitCode() == 1) {
				t.Fatalf("git config: %v\n%q", err, config)
			}
			return len(strings.Fields(string(out)))
		}
		config := cases["comment ending in backslash"]
		if got := count(includeConfig(config, "X", false)); got != 0 {
			t.Fatalf("include still read after removal: %d", got)
		}
		if got := count(includeConfig(config, "X", true)); got != 1 {
			t.Fatalf("include read %d times, want 1", got)
		}
	})
}

// TestConfigVariableValue reads a path as git does: includeConfig removes the
// variable for the value git reports, whatever whitespace, quotes and
// continuations the text spells it with.
func TestConfigVariableValue(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for raw, want := range map[string]string{
		"\\\n ovnfs-remotes":       "ovnfs-remotes",
		`"" ovnfs-remotes`:         "ovnfs-remotes",
		"ovnfs-remotes \\\n ; c":   "ovnfs-remotes ",
		"  ovnfs  \t remotes  # c": "ovnfs  \t remotes",
		`"a b" "c"`:                "a b c",
		`ovnfs-remotes \\ `:        `ovnfs-remotes \`,
		"ovnfs\\\r\n-remotes":      "ovnfs-remotes",
	} {
		config := "[include]\n\tpath = " + raw + "\n"
		file := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(file, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("git", "config", "--file", file, "--get", "include.path").Output()
		if err != nil {
			t.Fatalf("git config %q: %v", config, err)
		}
		if got := strings.TrimSuffix(string(out), "\n"); got != want {
			t.Fatalf("git reads %q as %q, test expects %q", config, got, want)
		}
		if got := includeConfig(config, want, false); got != "" {
			t.Errorf("includeConfig(%q, %q, false) = %q, want the include gone", config, want, got)
		}
	}
}
