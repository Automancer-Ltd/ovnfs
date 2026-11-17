package overlay

import (
	"os"

	"golang.org/x/sys/unix"
)

// attrPrefix namespaces the overlay's extended attributes.
const attrPrefix = "com.ovnfs."

// errNoXattr is the error reading or removing an absent attribute returns.
const errNoXattr = unix.ENOATTR

// refusesXattr reports nothing: APFS files of every type hold attributes.
func refusesXattr(error) bool { return false }

// reusesInodes reports whether the volume holding dir may give a new file the
// inode number of a removed one: APFS and HFS+ never do (HFS+ only after
// 2^32 files), so their entries keep their inodes' identities at no cost.
func reusesInodes(dir string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, &os.PathError{Op: "statfs", Path: dir, Err: err}
	}
	switch unix.ByteSliceToString(st.Fstypename[:]) {
	case "apfs", "hfs":
		return false, nil
	}
	return true, nil
}

// holdsAttr reports true: APFS and HFS+ files of every type hold attributes.
func holdsAttr(os.FileMode) bool { return true }
