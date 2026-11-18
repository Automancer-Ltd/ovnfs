package git

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Git LFS keeps large files outside the object database: the blob git tracks
// is a small pointer naming an object in the repository's LFS storage, and
// the lfs filter's smudge writes that object in its place on checkout.

// LFSPointerLimit bounds a pointer blob's size, as git-lfs does: a blob this
// large or larger is content, never a pointer.
const LFSPointerLimit = 1024

// LFSPointer names the object a Git LFS pointer blob stands for.
type LFSPointer struct {
	OID  string // the object's SHA-256, lowercase hex
	Size int64
}

// ParseLFSPointer reads a pointer blob in git-lfs's v1 format: a version line,
// then the sorted "oid sha256:HEX" and "size N" lines. Anything else is
// content: a malformed pointer, and one with extensions, whose own smudge
// commands decide what checkout writes.
func ParseLFSPointer(data []byte) (LFSPointer, bool) {
	text, ok := strings.CutSuffix(string(data), "\n")
	if len(data) >= LFSPointerLimit || !ok {
		return LFSPointer{}, false
	}
	lines := strings.Split(text, "\n")
	if len(lines) != 3 || lines[0] != "version https://git-lfs.github.com/spec/v1" && lines[0] != "version https://hawser.github.com/spec/v1" {
		return LFSPointer{}, false
	}
	hex, ok := strings.CutPrefix(lines[1], "oid sha256:")
	if !ok || len(hex) != 64 || strings.Trim(hex, "0123456789abcdef") != "" {
		return LFSPointer{}, false
	}
	value, ok := strings.CutPrefix(lines[2], "size ")
	size, err := strconv.ParseInt(value, 10, 64)
	if !ok || err != nil || size < 0 {
		return LFSPointer{}, false
	}
	return LFSPointer{OID: hex, Size: size}, true
}

// LFSCheckout reports whether git's checkout in r replaces pointers by their
// objects: the lfs filter driver has a process (preferred, as git does) or
// smudge command, and neither GIT_LFS_SKIP_SMUDGE nor the command's --skip
// (`git lfs install --skip-smudge`) leaves pointers in place.
func (r *Repo) LFSCheckout() (bool, error) {
	if skip, _ := envBool("GIT_LFS_SKIP_SMUDGE"); skip {
		return false, nil
	}
	cfg, err := r.repo.Config()
	if err != nil {
		return false, err
	}
	command, _ := cfg.Get("filter.lfs.process")
	if command == "" {
		command, _ = cfg.Get("filter.lfs.smudge")
	}
	return command != "" && !slices.Contains(strings.Fields(command), "--skip"), nil
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
// downloads an object it lacks into it. A failure carries git-lfs's stderr.
func (r *Repo) SmudgeLFS(ctx context.Context, p string, pointer []byte, w io.Writer) error {
	program, ok := gitProgram()
	if !ok {
		return errors.New("git is not installed to run git lfs smudge")
	}
	return r.runGit(ctx, program, gitRun{in: bytes.NewReader(pointer), out: w}, "lfs", "smudge", "--", p)
}
