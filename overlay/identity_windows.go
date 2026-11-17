//go:build windows

package overlay

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// nativeIdentity: Windows file information carries no file index, so every
// host file's metadata is an [idInfo].
const nativeIdentity = false

// fileIndexBits keeps an NTFS file index's record number, dropping the
// sequence number in its top 16 bits: a reused record then reads as a reused
// inode does on Unix, and the value fits an identity.
const fileIndexBits = 48

// nativeKey finds no inode in Windows file information; [describe] queries it.
func nativeKey(os.FileInfo) (inodeKey, bool) { return inodeKey{}, false }

// fileKey reads the volume serial number and file index of host file name
// without following a reparse point.
func fileKey(name string, _ os.FileInfo) (inodeKey, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return inodeKey{}, err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return inodeKey{}, &fs.PathError{Op: "inode", Path: name, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return inodeKey{}, &fs.PathError{Op: "inode", Path: name, Err: err}
	}
	return inodeKey{device: uint64(info.VolumeSerialNumber), inode: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)}, nil
}

// stateValue is the [ident.State] value of a file: its record number.
func stateValue(key inodeKey) uint64 { return key.inode & (1<<fileIndexBits - 1) }

// reusesInodes reports true: NTFS and ReFS reuse file records, whose
// sequence numbers [stateValue] drops.
func reusesInodes(string) (bool, error) { return true, nil }

// holdsAttr reports true: every file record holds alternate data streams.
func holdsAttr(os.FileMode) bool { return true }

// hardLinks reports 1: Windows file information has no link count, so a
// copy of one hard link of a frozen layer file takes over its identity.
func hardLinks(os.FileInfo) uint64 { return 1 }

// attrStream is the alternate data stream holding attribute a (see modeStream).
func attrStream(a string) string { return ":ovnfs." + a }

// readAttr returns attribute a of host file name; 0 when the file has none.
func readAttr(name, a string) (uint64, error) {
	stream, err := windows.UTF16PtrFromString(name + attrStream(a))
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(stream, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return 0, nil
	}
	if err != nil {
		return 0, &fs.PathError{Op: "open", Path: name + attrStream(a), Err: err}
	}
	defer windows.CloseHandle(h)
	var buf [9]byte
	var n uint32
	if err := windows.ReadFile(h, buf[:], &n, nil); err != nil {
		return 0, &fs.PathError{Op: "read", Path: name + attrStream(a), Err: err}
	}
	if n != 8 {
		return 0, &fs.PathError{Op: "read", Path: name + attrStream(a), Err: errors.New("overlay: malformed attribute stream")}
	}
	return binary.BigEndian.Uint64(buf[:8]), nil
}

// writeAttr replaces attribute a of host file name, leaving its timestamps
// and read-only attribute as they were. It fails with [errNoAttr] on
// filesystems without alternate data streams.
func writeAttr(name, a string, v uint64) error {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return &fs.PathError{Op: "setattr", Path: name, Err: err}
	}
	// The read-only attribute also forbids writing the file's streams.
	if attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
		if err := windows.SetFileAttributes(p, attrs&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
			return &fs.PathError{Op: "setattr", Path: name, Err: err}
		}
		defer windows.SetFileAttributes(p, attrs)
	}
	stream, err := windows.UTF16PtrFromString(name + attrStream(a))
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(stream, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if errors.Is(err, windows.ERROR_INVALID_NAME) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return errNoAttr
	}
	if err != nil {
		return &fs.PathError{Op: "setattr", Path: name + attrStream(a), Err: err}
	}
	defer windows.CloseHandle(h)
	// All-ones times suspend the updates this handle's write would make.
	keep := windows.Filetime{LowDateTime: 0xFFFFFFFF, HighDateTime: 0xFFFFFFFF}
	if err := windows.SetFileTime(h, nil, &keep, &keep); err != nil {
		return &fs.PathError{Op: "setattr", Path: name + attrStream(a), Err: err}
	}
	if err := windows.SetEndOfFile(h); err != nil {
		return &fs.PathError{Op: "setattr", Path: name + attrStream(a), Err: err}
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	var n uint32
	if err := windows.WriteFile(h, buf[:], &n, nil); err != nil {
		return &fs.PathError{Op: "setattr", Path: name + attrStream(a), Err: err}
	}
	return nil
}

// removeAttr deletes attribute a's stream of host file name, if it has one,
// leaving its read-only attribute as it was.
func removeAttr(name, a string) error {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return &fs.PathError{Op: "setattr", Path: name, Err: err}
	}
	if attrs&windows.FILE_ATTRIBUTE_READONLY != 0 {
		if err := windows.SetFileAttributes(p, attrs&^windows.FILE_ATTRIBUTE_READONLY); err != nil {
			return &fs.PathError{Op: "setattr", Path: name, Err: err}
		}
		defer windows.SetFileAttributes(p, attrs)
	}
	stream, err := windows.UTF16PtrFromString(name + attrStream(a))
	if err != nil {
		return err
	}
	err = windows.DeleteFile(stream)
	if err == nil || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_INVALID_NAME) {
		return nil
	}
	return &fs.PathError{Op: "setattr", Path: name + attrStream(a), Err: err}
}
