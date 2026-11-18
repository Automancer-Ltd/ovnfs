package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/stencil-hq/ovnfs/git/internal/config"
	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

// Git LFS keeps large files outside the object database: the blob git tracks
// is a small pointer naming an object in the repository's LFS storage, and
// the lfs filter's smudge writes that object in its place on checkout.

// LFSPointerLimit bounds a pointer blob's size, as git-lfs does: a blob this
// large or larger is content, never a pointer.
const LFSPointerLimit = 1024

// lfsVersions are the version lines git-lfs accepts, oldest last.
var lfsVersions = []string{"https://git-lfs.github.com/spec/v1", "https://hawser.github.com/spec/v1", "http://git-media.io/v/2"}

// LFSPointer names the object a Git LFS pointer blob stands for.
type LFSPointer struct {
	OID  string // the object's SHA-256, lowercase hex
	Size int64
}

// ParseLFSPointer decodes a pointer blob as git-lfs does: surrounding
// whitespace, CR line ends and blank lines are ignored, and the version, oid
// and size lines come in that order. A pointer with extensions, whose own
// smudge commands decide what checkout writes, counts as content, as does an
// oid that is not lowercase hex.
func ParseLFSPointer(data []byte) (LFSPointer, bool) {
	if len(data) >= LFSPointerLimit {
		return LFSPointer{}, false
	}
	keys := [...]string{"version", "oid", "size"}
	var values [len(keys)]string
	n := 0
	for line := range strings.SplitSeq(string(bytes.TrimSpace(data)), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok || n == len(keys) || key != keys[n] {
			return LFSPointer{}, false
		}
		values[n] = value
		n++
	}
	if n != len(keys) || !slices.Contains(lfsVersions, values[0]) {
		return LFSPointer{}, false
	}
	oid, ok := strings.CutPrefix(values[1], "sha256:")
	if !ok || len(oid) != sha256.Size*2 || strings.Trim(oid, "0123456789abcdef") != "" {
		return LFSPointer{}, false
	}
	size, err := strconv.ParseInt(values[2], 10, 64)
	if err != nil || size < 0 {
		return LFSPointer{}, false
	}
	return LFSPointer{OID: oid, Size: size}, true
}

// String is p's canonical encoding, the pointer git-lfs's clean filter writes.
func (p LFSPointer) String() string {
	return "version " + lfsVersions[0] + "\noid sha256:" + p.OID + "\nsize " + strconv.FormatInt(p.Size, 10) + "\n"
}

// Names reports whether r yields exactly the object p names: its size and
// SHA-256.
func (p LFSPointer) Names(r io.Reader) (bool, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return false, err
	}
	return n == p.Size && hex.EncodeToString(h.Sum(nil)) == p.OID, nil
}

// LFSFilter is what git's checkout in a repository does with LFS pointers:
// which paths the lfs filter writes the objects of, as git-lfs smudge decides.
type LFSFilter struct {
	include, exclude []ignoreRule
	fold             bool // paths match case-insensitively, as git-lfs does on macOS and Windows
}

// LFSFilter returns how git's checkout in r smudges LFS pointers, nil when it
// leaves them all in place: unless the lfs filter driver's process (preferred,
// as git does) or smudge command is git-lfs's own without its skip flag, and
// GIT_LFS_SKIP_SMUDGE is not true in this process's environment, which git
// passes on to git-lfs.
func (r *Repo) LFSFilter() (*LFSFilter, error) {
	if lfsBool(os.Getenv("GIT_LFS_SKIP_SMUDGE")) {
		return nil, nil
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return nil, err
	}
	command, _ := cfg.Get("filter.lfs.process")
	subcommand := "filter-process"
	if command == "" {
		command, _ = cfg.Get("filter.lfs.smudge")
		subcommand = "smudge"
	}
	if !runsGitLFS(command, subcommand) {
		return nil, nil
	}
	lfsconfig := r.lfsConfig()
	get := func(key string) string {
		if value, ok := cfg.Get(key); ok || lfsconfig == nil {
			return value
		}
		value, _ := lfsconfig.Get(key)
		return value
	}
	fold := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	return &LFSFilter{include: lfsPatterns(get("lfs.fetchinclude"), fold), exclude: lfsPatterns(get("lfs.fetchexclude"), fold), fold: fold}, nil
}

// lfsConfig reads the .lfsconfig git-lfs takes settings from, below git's
// configuration: the work tree's, else the index's, else HEAD's. One it cannot
// parse sets nothing.
func (r *Repo) lfsConfig() *config.Config {
	if r.Top != "" {
		name := filepath.Join(r.Top, ".lfsconfig")
		if c, err := config.ReadFile(name); err == nil || !errors.Is(err, os.ErrNotExist) {
			return c
		}
	}
	var c *config.Config
	r.sessionObjects(func(s *rev.Source, objects *repo.Objects) error {
		for _, spec := range []string{":.lfsconfig", "HEAD:.lfsconfig"} {
			id, err := rev.Resolve(s, spec, rev.Options{})
			if err != nil {
				continue
			}
			if kind, data, err := objects.Read(id); err == nil && kind == object.Blob {
				c, _ = config.Parse(data, spec)
				return nil
			}
		}
		return nil
	})
	return c
}

// Smudges reports whether checkout writes the object of the pointer at p
// rather than the pointer: lfs.fetchinclude, when set, names p and
// lfs.fetchexclude does not.
func (f *LFSFilter) Smudges(p string) bool {
	if f.fold {
		p = strings.ToLower(p)
	}
	return (len(f.include) == 0 || lfsMatch(f.include, p)) && !lfsMatch(f.exclude, p)
}

// runsGitLFS reports a filter command, as git's shell splits it, that runs
// git-lfs's subcommand (git-lfs, a path to it, or git lfs) without its skip
// flag (-s, --skip, --skip=true), which `git lfs install --skip-smudge` adds.
func runsGitLFS(command, subcommand string) bool {
	args := shellWords(command)
	if len(args) == 0 {
		return false
	}
	switch name := strings.TrimSuffix(strings.ToLower(path.Base(strings.ReplaceAll(args[0], `\`, "/"))), ".exe"); {
	case name == "git-lfs":
		args = args[1:]
	case name == "git" && len(args) > 1 && args[1] == "lfs":
		args = args[2:]
	default:
		return false
	}
	if len(args) == 0 || args[0] != subcommand {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--" {
			break
		}
		if arg == "-s" || arg == "--skip" {
			return false
		}
		for _, prefix := range []string{"--skip=", "-s="} {
			if value, ok := strings.CutPrefix(arg, prefix); ok {
				if skip, err := strconv.ParseBool(value); err != nil || skip {
					return false
				}
			}
		}
	}
	return true
}

// shellWords splits command as the POSIX shell git runs filter commands with:
// at blanks outside quotes, with single quotes literal, and backslashes
// escaping outside them (in double quotes, only before $, `, ", \ and newline).
func shellWords(command string) []string {
	var words []string
	var word strings.Builder
	inWord := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words, inWord = append(words, word.String()), false
				word.Reset()
			}
			continue
		case c == '\'':
			end := strings.IndexByte(command[i+1:], '\'')
			if end < 0 {
				end = len(command) - i - 1
			}
			word.WriteString(command[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			for i++; i < len(command) && command[i] != '"'; i++ {
				if command[i] == '\\' && i+1 < len(command) && strings.IndexByte("$`\"\\\n", command[i+1]) >= 0 {
					i++
				}
				word.WriteByte(command[i])
			}
		case c == '\\' && i+1 < len(command):
			i++
			word.WriteByte(command[i])
		default:
			word.WriteByte(c)
		}
		inWord = true
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// lfsBool reads a git-lfs boolean setting: true only for 1, t, true, on and yes.
func lfsBool(value string) bool {
	switch strings.ToLower(value) {
	case "1", "t", "true", "on", "yes":
		return true
	}
	return false
}

// lfsPatterns compiles a git-lfs path list: comma-separated gitignore
// patterns, of which *, *.* and . match everything, and a trailing slash
// does not restrict a pattern to directories; folded to lower case for fold.
func lfsPatterns(list string, fold bool) []ignoreRule {
	var rules []ignoreRule
	for pattern := range strings.SplitSeq(list, ",") {
		switch pattern = strings.TrimSpace(pattern); pattern {
		case "":
			continue
		case "*", "*.*", ".", "./", `.\`:
			pattern = "**"
		}
		if fold {
			pattern = strings.ToLower(pattern)
		}
		if pattern = strings.TrimRight(pattern, "/"); pattern != "" {
			rules = append(rules, newPatternRule(pattern, nil))
		}
	}
	return rules
}

// lfsMatch reports whether a rule matches the slash path p or a directory
// above it, whose contents a match covers.
func lfsMatch(rules []ignoreRule, p string) bool {
	parts := strings.Split(p, "/")
	for i := range rules {
		for n := 1; n <= len(parts); n++ {
			if rules[i].matches(parts[:n], n < len(parts)) {
				return true
			}
		}
	}
	return false
}

// LFSObject returns where r's LFS storage keeps the object with SHA-256 oid:
// below lfs.storage, which git-lfs takes literally (no ~) and relative to the
// common directory, else the common directory's lfs.
func (r *Repo) LFSObject(oid string) (string, error) {
	if len(oid) < 4 {
		return "", errors.New("invalid LFS object id " + strconv.Quote(oid))
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return "", err
	}
	storage, _ := cfg.Get("lfs.storage")
	switch {
	case storage == "":
		storage = filepath.Join(r.CommonDir, "lfs")
	case !filepath.IsAbs(storage):
		storage = filepath.Join(r.CommonDir, storage)
	}
	return filepath.Join(storage, "objects", oid[:2], oid[2:4], oid), nil
}

// SmudgeLFS writes to w what git's checkout writes for the work-tree path p
// whose blob is pointer: `git lfs smudge`, which reads r's LFS storage and
// downloads an object it lacks into it. A failure carries git-lfs's stderr;
// cancelling ctx ends git-lfs too.
func (r *Repo) SmudgeLFS(ctx context.Context, p string, pointer []byte, w io.Writer) error {
	program, ok := gitProgram()
	if !ok {
		return errors.New("git is not installed to run git lfs smudge")
	}
	return r.runGit(ctx, program, gitRun{in: bytes.NewReader(pointer), out: w}, "lfs", "smudge", "--", p)
}
