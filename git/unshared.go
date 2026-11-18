package git

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"time"

	"github.com/stencil-hq/ovnfs/git/internal/object"
	"github.com/stencil-hq/ovnfs/git/internal/repo"
)

// Unshared describes the commits made in a checkout that only it keeps.
type Unshared struct {
	Commits int    // how many there are
	Tip     string // the one its HEAD names, else the first a branch reaches
	Subject string // Tip's subject line
}

// Unshared counts the commits made in checkout name, whose private git
// directory is gitdir (see MakeGitdir), that removing it would lose: those
// its HEAD or a branch reaches and nothing else keeps. A commit is kept by any
// ref of r that reaches it, except the tracking refs of r's remote for this
// checkout, which go with it (see SetCheckoutRemotes), and by the checkout's
// own tracking refs of other remotes, which a push or a fetch there moved.
//
// Only commits in the checkout's own object store count. One it borrows from
// r was not made there, wherever r's refs have moved since, and one copied
// into r's store that no ref reaches is still only the checkout's.
func (r *Repo) Unshared(ctx context.Context, name, gitdir string) (Unshared, error) {
	src, err := repo.Open(gitdir)
	if err != nil {
		return Unshared{}, err
	}
	db, err := src.OpenDB()
	if err != nil {
		return Unshared{}, err
	}
	defer db.Close()

	// HEAD first, so that it names the tip when it holds such a commit.
	var tips []object.ID
	head, err := src.Refs().Resolve("HEAD")
	switch {
	case err == nil:
		tips = append(tips, head.ID)
	case !errors.Is(err, fs.ErrNotExist):
		return Unshared{}, err
	}
	branches, err := src.Refs().List("refs/heads/")
	if err != nil {
		return Unshared{}, err
	}
	for _, b := range branches {
		if !b.Symbolic() {
			tips = append(tips, b.ID)
		}
	}
	made := make(map[object.ID]*object.CommitObject)
	var order []object.ID
	var oldest time.Time
	for _, tip := range tips {
		for stack := []object.ID{tip}; len(stack) > 0; {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if made[id] != nil || !db.ContainsOwn(id) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return Unshared{}, err
			}
			kind, data, err := db.Read(id)
			if err != nil {
				return Unshared{}, err
			}
			if kind != object.Commit {
				continue // a branch may name a tag or a tree; neither is a commit to lose
			}
			commit, err := object.ParseCommit(db.Format(), data)
			if err != nil {
				return Unshared{}, err
			}
			made[id] = commit
			order = append(order, id)
			// A time that did not parse is the zero one, which bounds nothing.
			if when := commit.Committer.When; len(order) == 1 || when.Before(oldest) {
				oldest = when
			}
			stack = append(stack, commit.Parents...)
		}
	}
	if len(order) == 0 {
		return Unshared{}, nil
	}

	kept, err := r.keptCommits(ctx, db, src, name, made, oldest)
	if err != nil {
		return Unshared{}, err
	}
	var out Unshared
	for _, id := range order {
		if kept[id] {
			continue
		}
		if out.Commits++; out.Tip == "" {
			out.Tip = id.String()
			out.Subject, _, _ = strings.Cut(strings.TrimLeft(made[id].Message, "\n"), "\n")
		}
	}
	return out, nil
}

// keptCommits returns which of made, the commits Unshared found in a
// checkout's own store, the refs that outlive the checkout reach. The walk
// from those refs stops at a commit older than oldest, the oldest of made: a
// commit's descendants are not older than it.
func (r *Repo) keptCommits(ctx context.Context, db *object.DB, src *repo.Repository, name string, made map[object.ID]*object.CommitObject, oldest time.Time) (map[object.ID]bool, error) {
	// Refs naming one of made come last, to be walked first: once all of made
	// is kept, as after a land, the other refs are never read.
	var roots, direct []object.ID
	add := func(id object.ID) {
		if made[id] != nil {
			direct = append(direct, id)
		} else {
			roots = append(roots, id)
		}
	}
	folder, err := r.repo.Refs().List("refs/")
	if err != nil {
		return nil, err
	}
	doomed := trackingPrefix(name)
	for _, ref := range folder {
		if !ref.Symbolic() && !strings.HasPrefix(ref.Name, doomed) {
			add(ref.ID)
		}
	}
	head, err := r.repo.Refs().Resolve("HEAD")
	switch {
	case err == nil:
		add(head.ID)
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	pushed, err := src.Refs().List("refs/remotes/")
	if err != nil {
		return nil, err
	}
	for _, ref := range pushed {
		// A copy of r's tracking ref for a checkout keeps nothing.
		if !ref.Symbolic() && !strings.HasPrefix(ref.Name, "refs/remotes/"+remotePrefix) {
			add(ref.ID)
		}
	}

	kept := make(map[object.ID]bool)
	seen := make(map[object.ID]bool)
	for stack := append(roots, direct...); len(stack) > 0 && len(kept) < len(made); {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if commit := made[id]; commit != nil {
			kept[id] = true
			stack = append(stack, commit.Parents...)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// db reads r's objects through alternates.
		kind, data, err := db.Read(id)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a ref to an object r was only promised
		}
		if err != nil {
			return nil, err
		}
		switch kind {
		case object.Tag:
			tag, err := object.ParseTag(db.Format(), data)
			if err != nil {
				return nil, err
			}
			stack = append(stack, tag.Object)
		case object.Commit:
			commit, err := object.ParseCommit(db.Format(), data)
			if err != nil {
				return nil, err
			}
			if when := commit.Committer.When; when.IsZero() || !when.Before(oldest) {
				stack = append(stack, commit.Parents...)
			}
		}
	}
	return kept, nil
}
