package nfs

// keptKey names an NFSv3 kept file (v3State.kept). It separates cached
// writers, keyed by id and current path, from permission fallbacks, whose
// empty path lets them follow renames like an already-open file. Cached
// writers reopen after a rename so a backend that reopens descriptors across
// snapshots remembers the current path.
type keptKey struct {
	id   uint64
	path string
}
