package overlay

import (
	"os"
	"path"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// inodeKey identifies one host inode so hardlinked paths share metadata updates.
type inodeKey struct{ device, inode uint64 }

// inodeNames stores ordinary single-linked files without a per-file map allocation.
type inodeNames struct {
	first  string
	others map[string]struct{}
}

// addIdentity records a name, allocating an alias set only for real hardlinks.
func (f *FS) addIdentity(p string, key inodeKey) {
	names := f.aliases[key]
	if names.first == "" {
		names.first = p
	} else if names.first != p {
		if names.others == nil {
			names.others = make(map[string]struct{})
		}
		names.others[p] = struct{}{}
	}
	f.aliases[key] = names
	f.pathIDs[p] = key
}

// track records one upper name's identity and, for a host file, its inode,
// without reading or changing its metadata.
func (f *FS) track(p string, info os.FileInfo) {
	if info == nil {
		return
	}
	f.register(p, info)
	if info.IsDir() {
		return
	}
	key, ok := hostKey(info)
	if !ok {
		return
	}
	if old, ok := f.pathIDs[p]; ok {
		if old == key {
			return
		}
		f.untrack(p)
	}
	f.addIdentity(p, key)
}

// register makes upper name p, described by info, where [FS.Locate] finds
// its identity, unless another indexed name still holds it (a hard link).
func (f *FS) register(p string, info os.FileInfo) {
	id, ok := ident.Of(info)
	if !ok || f.paths == nil {
		return
	}
	if cur, ok := f.paths[id]; ok && cur != p {
		if e, live := f.index.entries[cur]; live && e.Info != nil {
			if held, _ := ident.Of(e.Info); held == id {
				return
			}
		}
	}
	f.paths[id] = p
}

// unregister forgets upper name p, about to leave the index, as the holder of
// its identity, handing it to another name of the same file if one remains,
// and forgets p's inode in the identity cache: the host may hand its number
// out again once no name is left.
func (f *FS) unregister(p string) {
	e, ok := f.index.entries[p]
	if !ok || e.Info == nil {
		return
	}
	if key, ok := hostKey(e.Info); ok {
		f.ids.known.forget(key)
	}
	id, ok := ident.Of(e.Info)
	if !ok || f.paths[id] != p {
		return
	}
	delete(f.paths, id)
	if other := f.otherAlias(p); other != "" {
		f.paths[id] = other
	} else if m := f.files[p]; m != nil {
		if other := m.otherName(p); other != "" {
			f.paths[id] = other
		}
	}
}

// untrack retires one upper name without retaining dead inode groups.
func (f *FS) untrack(p string) {
	key, ok := f.pathIDs[p]
	if !ok {
		return
	}
	delete(f.pathIDs, p)
	names := f.aliases[key]
	if names.first == p {
		names.first = ""
		for other := range names.others {
			names.first = other
			delete(names.others, other)
			break
		}
	} else {
		delete(names.others, p)
	}
	if names.first == "" {
		delete(f.aliases, key)
	} else {
		if len(names.others) == 0 {
			names.others = nil
		}
		f.aliases[key] = names
	}
}

// dropIndex removes a subtree and all its hardlink bookkeeping together.
func (f *FS) dropIndex(p string) {
	for n := range f.index.children[p] {
		f.dropIndex(path.Join(p, n))
	}
	f.unregister(p)
	if m := f.files[p]; m != nil {
		f.unname(m, p)
	}
	f.untrack(p)
	delete(f.unsettled, p)
	f.index.drop(p)
}

// otherAlias finds a surviving name whose link count changes when p is removed.
func (f *FS) otherAlias(p string) string {
	key, ok := f.pathIDs[p]
	if !ok {
		return ""
	}
	names := f.aliases[key]
	if names.first != p {
		return names.first
	}
	for other := range names.others {
		return other
	}
	return ""
}

// moveIdentity registers a renamed path, as an inode name and as its
// identity's holder, before the old name retires.
func (f *FS) moveIdentity(from, to string) {
	if key, ok := f.pathIDs[from]; ok {
		f.addIdentity(to, key)
	}
	if e := f.index.entries[from]; e.Info != nil {
		if id, ok := ident.Of(e.Info); ok && f.paths[id] == from {
			f.paths[id] = to
		}
	}
}

// restat re-reads p's metadata after a descriptor changed the inode's content
// or attributes and shares it with p's other hard-link names; the inode, and
// so the identity bookkeeping, is unchanged.
func (f *FS) restat(p string) error {
	info, err := f.stat(p)
	if err != nil {
		return err
	}
	f.setInfo(p, info)
	f.cacheAliases(p, info)
	return nil
}

// setInfo indexes upper name p's metadata, keeping its opacity and redirect.
func (f *FS) setInfo(p string, info os.FileInfo) {
	e := entry(info)
	old := f.index.entries[p]
	e.Opaque = old.Opaque
	if e.Kind == KindDir {
		e.Redirect = old.Redirect
	}
	f.index.set(p, e)
}

// cacheInfo stores a fresh snapshot and shares it with other names of the same inode.
func (f *FS) cacheInfo(p string, info os.FileInfo) {
	f.setInfo(p, info)
	f.track(p, info)
	f.cacheAliases(p, info)
}

// cacheAliases gives p's other hard-link names info, p's fresh metadata. The
// names share one inode, so only the name differs; re-reading each would cost
// a host lstat per link on every metadata change.
func (f *FS) cacheAliases(p string, info os.FileInfo) {
	key, ok := f.pathIDs[p]
	if !ok {
		return
	}
	names := f.aliases[key]
	info = unalias(info)
	if names.first != p {
		f.setInfo(names.first, aliasInfo{FileInfo: info, name: path.Base(names.first)})
	}
	for other := range names.others {
		if other != p {
			f.setInfo(other, aliasInfo{FileInfo: info, name: path.Base(other)})
		}
	}
}

// aliasInfo is a file's metadata read through another of its names: a hard
// link's, or a spilled file's staged before a rename gave it its name.
type aliasInfo struct {
	os.FileInfo
	name string
}

// Name returns the alias's own base name.
func (i aliasInfo) Name() string { return i.name }

// Identity forwards the identity of the file the alias names.
func (i aliasInfo) Identity() ident.ID { id, _ := ident.Of(i.FileInfo); return id }

// unalias returns the metadata beneath an [aliasInfo].
func unalias(info os.FileInfo) os.FileInfo {
	if a, ok := info.(aliasInfo); ok {
		return a.FileInfo
	}
	return info
}

// bind associates an open descriptor with its inode, not its current name; a
// writable one makes the inode's indexed metadata volatile until it closes.
func (f *FS) bind(h *file, p string) {
	h.id = handleID(f.index.entries[p].Info)
	h.inode, h.tracked = f.pathIDs[p]
	if h.writable && h.tracked {
		f.writers[h.inode]++
	}
}
