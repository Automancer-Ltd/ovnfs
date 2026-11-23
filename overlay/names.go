package overlay

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Names the upper's volume cannot hold apart. The index keeps names as
// bytes, but a volume may refuse some (APFS: invalid UTF-8) and take two
// different ones for one entry (APFS: names differing only in Unicode
// normalization, and in case on a case-insensitive volume). With the memory
// tier and the queue, a name reaches the volume after the call returned, so
// such a name would fail there for good or land on another entry. Every
// name-creating call therefore asks [FS.claimName] first and answers as
// the host would: EILSEQ for a name the volume refuses, EEXIST for a name
// it takes for another existing entry of the directory. The host's own
// create without O_EXCL would open that other entry, and its rename would
// replace it; both are refused with EEXIST instead, while lookups keep
// matching names exactly, as they do in the synchronous path.

// nameRule is what the upper's volume does with names, found by
// [probeNames] when the overlay opens.
type nameRule struct {
	foldCase      bool // names differing only in case are one entry
	foldNorm      bool // names differing only in Unicode normalization are one entry
	rejectInvalid bool // names that are not valid UTF-8 are refused
	// foldInvalid: names that are not valid UTF-8 are one entry with every
	// invalid byte read as U+FFFD, as Go spells them in UTF-16 on Windows.
	foldInvalid bool
}

// folds reports whether the volume takes some different names for one entry.
func (r nameRule) folds() bool { return r.foldCase || r.foldNorm || r.foldInvalid }

// key returns the form of name under which the volume finds an entry:
// equal keys name one entry.
func (r nameRule) key(name string) string {
	if r.foldInvalid && !utf8.ValidString(name) {
		name = string([]rune(name)) // U+FFFD for each invalid byte, as ranging over name reads it
	}
	if r.foldNorm {
		name = norm.NFD.String(name)
	}
	if r.foldCase {
		name = cases.Fold().String(name)
	}
	return name
}

// probeNames finds the rule of the volume holding dir, an empty private
// directory, by creating and looking up names in it.
func probeNames(dir string) (nameRule, error) {
	var r nameRule
	created := func(name string) error {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		return f.Close()
	}
	same := func(a, b string) (bool, error) {
		if err := created(a); err != nil {
			return false, err
		}
		_, err := os.Lstat(filepath.Join(dir, b))
		return err == nil, errors.Join(os.Remove(filepath.Join(dir, a)), ignoreMissing(err))
	}
	var err error
	if r.foldCase, err = same("probe-a", "PROBE-A"); err != nil {
		return r, err
	}
	if r.foldNorm, err = same("probe-\u00e9", "probe-e\u0301"); err != nil {
		return r, err
	}
	switch err := created("probe-\xff\xfe"); {
	case errors.Is(err, syscall.EILSEQ) || errors.Is(err, syscall.EINVAL):
		r.rejectInvalid = true
	case err != nil:
		return r, err
	default:
		_, err := os.Lstat(filepath.Join(dir, "probe-\xfe\xff"))
		r.foldInvalid = err == nil
		if err := errors.Join(os.Remove(filepath.Join(dir, "probe-\xff\xfe")), ignoreMissing(err)); err != nil {
			return r, err
		}
	}
	return r, nil
}

// ignoreMissing drops a not-exist error.
func ignoreMissing(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// claimName checks that the upper's volume can hold the new name p as an
// entry of its own in its directory: EILSEQ when the volume refuses the
// name, EEXIST when it would take it for an existing entry other than p
// itself and except (a rename's source). The caller holds f.mu with the
// index loaded.
func (f *FS) claimName(op, p, except string) error {
	base := path.Base(p)
	if f.volume.rejectInvalid && !utf8.ValidString(base) {
		return &fs.PathError{Op: op, Path: p, Err: syscall.EILSEQ}
	}
	if !f.volume.folds() {
		return nil
	}
	q, ok := f.index.folded[foldedPath(parent(p), f.volume.key(base))]
	if ok && q != p && q != except {
		return &fs.PathError{Op: op, Path: p, Err: syscall.EEXIST}
	}
	return nil
}

// foldedPath is the key of [Layer.folded] for an entry of directory dir
// whose name has key key.
func foldedPath(dir, key string) string { return dir + "/" + key }
