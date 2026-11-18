package checkout

import (
	"context"
	"errors"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
)

// Land puts a checkout's commits on the branch checked out in its folder: what
// `git commit` there would have left. The branch fast-forwards to the
// checkout's HEAD, its index follows for the paths those commits changed, and
// the checkout's files for those paths merge into the folder as an atomic
// Apply merges them, so the folder's own uncommitted edits stay on top.
//
// Everything that can refuse does so before the folder is written: a detached
// HEAD there, a branch holding commits the checkout's HEAD lacks, staged
// changes on the commits' paths, commits that cannot be copied, and files that
// would conflict (the reply then has Refused set), unless Markers asks for
// their conflict markers in the folder instead. The branch moves last, once
// every file is merged; until then, and should moving it still fail, the
// folder holds the merged files as uncommitted edits and landing again moves
// it.
func (r *Registry) Land(ctx context.Context, args api.LandArgs) (api.LandReply, error) {
	resume, err := r.Quiesce(ctx)
	if err != nil {
		return api.LandReply{}, err
	}
	defer resume()
	h, err := r.lookupCheckout(args.Checkout)
	if err != nil {
		return api.LandReply{}, err
	}
	name, meta := h.meta.Name, h.base.view.Meta()
	if meta.Remote != nil {
		return api.LandReply{}, failure("invalid", "checkout %s is attached from %s; its folder is not on this machine", name, meta.Remote.Source)
	}
	repo := h.base.view.Domains().Repo("")
	if !h.meta.Git || repo == nil {
		return api.LandReply{}, failure("invalid", "checkout %s has no git directory, so no commits to land; `ovnfs apply %s` merges its files", name, name)
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		return api.LandReply{}, coded(err)
	}
	if branch == "" {
		return api.LandReply{}, failure("conflict", "HEAD is detached in %s, so there is no branch to land on; `ovnfs apply %s --branch NAME` brings the commits as a branch", meta.Dir, name)
	}
	old, err := repo.Head(ctx)
	if err != nil {
		return api.LandReply{}, coded(err)
	}
	gitdir := r.gitdirPath(h.meta.dir())
	refused := func(err error) error {
		conflict, ok := errors.AsType[*git.BranchConflict](err)
		switch {
		case !ok:
			return codedTransfer(err)
		case conflict.Diverged:
			return failure("conflict", "%s in %s holds commits the checkout's are not based on; inside the checkout, `git fetch %s %s && git rebase --autostash FETCH_HEAD`, then land again",
				branch, meta.Dir, shellQuote(meta.Dir), branch)
		case len(conflict.Staged) != 0:
			return failure("conflict", "%s in %s, which the checkout's commits change too; commit or unstage them there, then land again", conflict, meta.Dir)
		}
		return failure("conflict", "%s in %s; land again", conflict, meta.Dir)
	}
	commit, changed, err := repo.FetchBranch(ctx, gitdir, branch, true)
	if err != nil {
		return api.LandReply{}, refused(err)
	}
	reply := api.LandReply{Old: old, ApplyReply: api.ApplyReply{Source: meta.Dir, Applied: []api.Change{}, Merged: []string{}, Conflicts: []api.Conflict{}, Branch: branch, Commit: commit}}
	if commit == old {
		return reply, nil
	}
	if !args.DryRun {
		// Copied before the folder is written: this is what its repository can refuse.
		if err := repo.FetchCommits(ctx, gitdir); err != nil {
			return api.LandReply{}, codedTransfer(err)
		}
	}
	only := make(map[string]bool, len(changed))
	for _, c := range changed {
		only[c.Path] = true
	}
	files, left, err := r.mergeFiles(ctx, h, mergeOptions{dryRun: args.DryRun, atomic: !args.Markers, only: only})
	if err != nil {
		return api.LandReply{}, err
	}
	files.Branch, files.Commit = branch, commit
	reply.ApplyReply, reply.Left = files, left
	if args.DryRun || len(files.Conflicts) != 0 {
		if len(files.Conflicts) != 0 {
			reply.Commit = old
		}
		return reply, nil
	}
	if _, _, err := repo.FetchBranch(ctx, gitdir, branch, false); err != nil {
		return api.LandReply{}, failure("conflict", "the checkout's files are merged into %s as uncommitted edits, but %s did not move: %v", meta.Dir, branch, refused(err))
	}
	return reply, nil
}
