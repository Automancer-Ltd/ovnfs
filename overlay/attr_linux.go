package overlay

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// attrPrefix puts the overlay's extended attributes in the user namespace,
// the only one unprivileged processes may write.
const attrPrefix = "user.ovnfs."

// errNoXattr is the error reading or removing an absent attribute returns.
const errNoXattr = unix.ENODATA

// refusesXattr reports the refusal of a user attribute on a symlink or
// special file: they exist only on regular files and directories.
func refusesXattr(err error) bool { return errors.Is(err, unix.EPERM) }

// reusesInodes reports true: ext4, xfs, btrfs and tmpfs all may give a new
// file the inode number of a removed one.
func reusesInodes(string) (bool, error) { return true, nil }

// holdsAttr reports whether a file of mode's type can hold a user extended
// attribute: only regular files and directories can.
func holdsAttr(mode os.FileMode) bool { return mode.IsRegular() || mode.IsDir() }
