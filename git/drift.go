package git

import (
	"context"

	"github.com/stencil-hq/ovnfs/git/internal/repo"
	"github.com/stencil-hq/ovnfs/git/internal/rev"
)

// Drift tells how the HEAD of a checkout's private git directory stands to
// its folder's.
type Drift struct {
	Folder, Checkout string // the two HEAD commits; "" for an unborn one
	// Behind reports that the checkout's HEAD does not hold the folder's: the
	// folder gained commits the checkout's history lacks.
	Behind bool
	// FastForward reports, for a checkout that is Behind, that the folder's
	// HEAD holds the checkout's, so moving the checkout's HEAD there loses no
	// commit of its own.
	FastForward bool
}

// Drift compares r's HEAD with the HEAD of gitdir, a checkout's private git
// directory (see MakeGitdir), which reads r's objects beside its own.
func (r *Repo) Drift(ctx context.Context, gitdir string) (Drift, error) {
	folder, err := r.Head(ctx)
	if err != nil {
		return Drift{}, err
	}
	src, err := repo.Open(gitdir)
	if err != nil {
		return Drift{}, err
	}
	objects, err := src.OpenObjects()
	if err != nil {
		return Drift{}, err
	}
	defer objects.Close()
	s, err := src.RevSource(objects)
	if err != nil {
		return Drift{}, err
	}
	checkout, err := resolveCommit(&s, "HEAD")
	if err != nil {
		return Drift{}, err
	}
	d := Drift{Folder: folder, Checkout: checkout}
	if folder == "" || checkout == "" || folder == checkout {
		return d, nil
	}
	theirs, ours, err := commitIDs(&s, folder, checkout)
	if err != nil {
		return Drift{}, err
	}
	held, err := rev.IsAncestor(&s, theirs, ours)
	if err != nil || held {
		return d, err
	}
	d.Behind = true
	d.FastForward, err = rev.IsAncestor(&s, ours, theirs)
	return d, err
}
