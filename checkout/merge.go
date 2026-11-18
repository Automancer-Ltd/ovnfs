package checkout

import (
	"bytes"
	"maps"
	"path"
	"slices"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/git"
	"github.com/stencil-hq/ovnfs/store"
)

// outcome is what apply or pull did with one changed path.
type outcome int

// Merge outcomes; skipped means the target already had the incoming version.
const (
	skipped outcome = iota
	applied
	merged
	marked     // written with conflict markers for the user to resolve
	unresolved // left untouched: it still holds conflict markers
	conflict   // left untouched
)

// The merge sources between a checkout and its folder (see folderBases and
// checkoutMeta.MergeBases).
const (
	applySource   = "apply"       // paths an apply left conflict markers for in the folder
	appliedSource = "applied"     // paths the folder took from the checkout in an apply
	folderSource  = "pull folder" // paths a pull from the folder left conflict markers for in the checkout
	pulledSource  = "pulled"      // paths the checkout took from the folder in a pull
)

// pullSource keys the merge bases of pulls from the lane attached as tree from.
func pullSource(from store.OID) string { return "pull " + string(from) }

// gone is the base of a path a checkout and its folder agreed is absent: an
// empty directory, which is no file to either of them (see compare).
var gone = store.File{Mode: store.ModeDir, ID: store.EmptyTree}

// folderBases are the versions of paths on which a checkout and its folder
// last agreed, by who took them. marked and applied hold the checkout's
// version of a path an apply merged into the folder, marked when it left
// conflict markers there; markedHere and pulled hold the folder's version of a
// path a pull merged into the checkout, markedHere when it left conflict
// markers there. A merge either way takes them as those paths' base in place
// of the capture's, so what one side already took is neither merged into it
// again nor mistaken for a change of its own: without them, a line applied
// and then edited again would conflict with itself. A pull forgets what an
// apply recorded for the paths it settles, so the apply side is the newer one
// wherever both name a path.
type folderBases struct {
	marked, applied, markedHere, pulled map[string]store.File
}

// baseSource is one map of a folderBases and the merge source it is stored under.
type baseSource struct {
	source string
	bases  *map[string]store.File
}

// sources lists b's maps with their merge sources.
func (b *folderBases) sources() [4]baseSource {
	return [4]baseSource{{applySource, &b.marked}, {appliedSource, &b.applied}, {folderSource, &b.markedHere}, {pulledSource, &b.pulled}}
}

// folderMergeBases returns the entries of a checkout's merge bases that
// describe its folder: a fork starts from the same files, so it starts from
// the same agreement with the folder.
func folderMergeBases(all map[string]string) map[string]string {
	var out map[string]string
	for _, s := range new(folderBases).sources() {
		if id := all[s.source]; id != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[s.source] = id
		}
	}
	return out
}

// folderBases loads the bases of merges between h and its folder.
func (r *Registry) folderBases(h *checkoutRecord) (folderBases, error) {
	var b folderBases
	for _, s := range b.sources() {
		m, err := r.mergeBases(h, s.source)
		if err != nil {
			return folderBases{}, err
		}
		*s.bases = m
	}
	return b, nil
}

// setFolderBases persists b as the bases of merges between h and its folder;
// read supplies the content of leaves the store lacks.
func (r *Registry) setFolderBases(h *checkoutRecord, b folderBases, read func(p string, f store.File) ([]byte, error)) error {
	all := make(map[string]map[string]store.File, 4)
	for _, s := range b.sources() {
		all[s.source] = *s.bases
	}
	return r.setMergeBases(h, all, read)
}

// over replaces the leaves of from, a capture's, by the bases a merge between
// the checkout and its folder starts from; without applied, only by what pulls
// brought into the checkout, which is the base its own changes count from.
func (b folderBases) over(from map[string]store.File, applied bool) {
	layers := []map[string]store.File{b.pulled, b.markedHere}
	if applied {
		layers = append(layers, b.applied, b.marked)
	}
	for _, bases := range layers {
		for p, f := range bases {
			if f.Mode == store.ModeDir {
				delete(from, p)
			} else {
				from[p] = f
			}
		}
	}
}

// clone returns a copy a trial merge may advance without touching b.
func (b folderBases) clone() folderBases {
	return folderBases{marked: maps.Clone(b.marked), applied: maps.Clone(b.applied), markedHere: maps.Clone(b.markedHere), pulled: maps.Clone(b.pulled)}
}

// setBase records f, or gone for an absent f, as p's base in bases.
func setBase(bases map[string]store.File, p string, f store.File) {
	if f.ID == "" || f.Mode == store.ModeDir {
		f = gone
	}
	bases[p] = f
}

// afterApply records what an apply did with p, whose version in the checkout is
// theirs: the folder took it, with conflict markers when marked. A path whose
// pull markers kept it out of applies until now has been resolved.
func (b folderBases) afterApply(p string, theirs store.File, o outcome) {
	switch o {
	case conflict, unresolved:
		return
	case marked:
		setBase(b.marked, p, theirs)
		delete(b.applied, p)
	default:
		setBase(b.applied, p, theirs)
		delete(b.marked, p)
	}
	if f, ok := b.markedHere[p]; ok {
		b.pulled[p] = f
		delete(b.markedHere, p)
	}
}

// afterPull records what a pull from the folder did with p, whose version in
// the folder is theirs: the checkout took it, with conflict markers when
// marked, which ends what applies recorded for p.
func (b folderBases) afterPull(p string, theirs store.File, o outcome) {
	switch o {
	case conflict, unresolved:
		return
	case marked:
		setBase(b.markedHere, p, theirs)
		delete(b.pulled, p)
	default:
		setBase(b.pulled, p, theirs)
		delete(b.markedHere, p)
	}
	delete(b.marked, p)
	delete(b.applied, p)
}

// sides names the two sides of a merge: where each changed, for conflict
// reasons, and how conflict markers label them.
type sides struct {
	ours, theirs string
	labels       git.Labels
}

// conflict says why a path both sides changed since base conflicts, and whether
// a text merge may still settle it.
func (s sides) conflict(base, ours, theirs store.File) (why string, text bool) {
	switch {
	case theirs.ID == "":
		return "deleted " + s.theirs + " but changed " + s.ours, false
	case ours.ID == "":
		return "changed " + s.theirs + " but deleted " + s.ours, false
	case base.ID == "":
		return "added in both with different content", regular(ours) && regular(theirs)
	}
	return "changed on both sides", regular(base) && regular(ours) && regular(theirs)
}

// regular reports whether a leaf is a file rather than a symlink or directory.
func regular(f store.File) bool { return f.Mode == store.ModeFile || f.Mode == store.ModeExec }

// mergeText merges a text file both sides changed. A clean merge comes back
// merged, or skipped when ours already holds it; overlapping changes come back
// marked, with the conflict markers to write. Ours still holding markers stays
// unresolved, and content git cannot merge stays a conflict, why explaining it.
func mergeText(labels git.Labels, ours, base, theirs []byte, why string) ([]byte, outcome, string) {
	if hasMarkers(ours) {
		return nil, unresolved, "conflict markers are not resolved yet"
	}
	data, clean, err := git.MergeFile(ours, base, theirs, labels)
	switch {
	case err != nil:
		return nil, conflict, why + " (" + err.Error() + ")"
	case !clean:
		return data, marked, why
	case bytes.Equal(ours, data):
		return nil, skipped, ""
	}
	return data, merged, ""
}

// hasMarkers reports whether data holds an unresolved conflict: a line opening
// one and a later line closing it, as git merge-file writes them.
func hasMarkers(data []byte) bool {
	open := false
	for line := range bytes.Lines(data) {
		switch {
		case bytes.HasPrefix(line, []byte("<<<<<<< ")):
			open = true
		case open && bytes.HasPrefix(line, []byte(">>>>>>> ")):
			return true
		}
	}
	return false
}

// report adds one path's outcome to a merge reply.
func report(reply *api.ApplyReply, c api.Change, o outcome, reason string) {
	switch o {
	case applied:
		reply.Applied = append(reply.Applied, c)
	case merged:
		reply.Merged = append(reply.Merged, c.Path)
	case marked, unresolved:
		reply.Conflicts = append(reply.Conflicts, api.Conflict{Path: c.Path, Reason: reason, Markers: true})
	case conflict:
		reply.Conflicts = append(reply.Conflicts, api.Conflict{Path: c.Path, Reason: reason})
	}
}

// reportPending reports the based paths a merge did not reach whose target
// still holds conflict markers, so they keep asking to be resolved.
func reportPending(reply *api.ApplyReply, bases map[string]store.File, seen map[string]bool, hasMarkersAt func(p string) (bool, error)) error {
	for _, p := range slices.Sorted(maps.Keys(bases)) {
		if seen[p] {
			continue
		}
		ok, err := hasMarkersAt(p)
		if err != nil {
			return err
		}
		if ok {
			report(reply, api.Change{Path: p}, unresolved, "conflict markers are not resolved yet")
		}
	}
	return nil
}

// advance keeps bases current once a merge settled p. A path marked now, or
// one marked earlier that the merge got past, takes theirs as its base: the
// target holds that version's changes, resolved by the user, so the next merge
// starts from it rather than merging them again.
func advance(bases map[string]store.File, p string, theirs store.File, o outcome) {
	_, based := bases[p]
	switch {
	case o == conflict || o == unresolved || !based && o != marked:
	case theirs.ID == "" || theirs.Mode == store.ModeDir:
		delete(bases, p)
	default:
		bases[p] = theirs
	}
}

// mergeBases returns the leaves that replace the base of source's merges into h.
func (r *Registry) mergeBases(h *checkoutRecord, source string) (map[string]store.File, error) {
	r.mu.RLock()
	id := h.meta.MergeBases[source]
	r.mu.RUnlock()
	if id == "" {
		return map[string]store.File{}, nil
	}
	return r.store.Flatten(store.OID(id))
}

// setMergeBases persists, in one state write, each entry of all as the merge
// bases of its source's merges into h, first storing with read any leaf
// content the store lacks. A base lying above another one (a file where a
// later merge found a directory) is dropped: a tree holds only one of the two,
// and the path's capture is a base still.
func (r *Registry) setMergeBases(h *checkoutRecord, all map[string]map[string]store.File, read func(p string, f store.File) ([]byte, error)) error {
	ids := make(map[string]string, len(all))
	for source, bases := range all {
		for p := range bases {
			for up := path.Dir(p); up != "."; up = path.Dir(up) {
				delete(bases, up)
			}
		}
		if len(bases) == 0 {
			ids[source] = ""
			continue
		}
		for p, f := range bases {
			if f.Mode == store.ModeDir || r.store.Has(f.ID) {
				continue
			}
			data, err := read(p, f)
			if err != nil {
				return err
			}
			if _, err := r.store.PutBytes(data); err != nil {
				return err
			}
		}
		tree, err := r.store.Build(bases)
		if err != nil {
			return err
		}
		ids[source] = string(tree)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return failure("io", "registry is closed")
	}
	old := maps.Clone(h.meta.MergeBases)
	changed := false
	for source, id := range ids {
		if id == h.meta.MergeBases[source] {
			continue
		}
		changed = true
		if id == "" {
			delete(h.meta.MergeBases, source)
			continue
		}
		if h.meta.MergeBases == nil {
			h.meta.MergeBases = map[string]string{}
		}
		h.meta.MergeBases[source] = id
	}
	if !changed {
		return nil
	}
	if err := r.persistLocked(); err != nil {
		h.meta.MergeBases = old
		return err
	}
	return nil
}
