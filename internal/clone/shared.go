package clone

// Shared clones the hierarchy at src to the new path dst as [Tree] does, but
// never copies bytes: where a regular file's blocks cannot be shared it stops
// with an error wrapping errors.ErrUnsupported, leaving whatever it already
// cloned at dst for the caller to remove. Base cache snapshots use it, so a
// build directory of gigabytes on a volume without cloning is never copied.
func Shared(src, dst string) error { return hierarchy(src, dst, walk{shared: true}) }
