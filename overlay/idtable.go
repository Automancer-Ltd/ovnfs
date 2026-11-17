package overlay

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/stencil-hq/ovnfs/internal/ident"
)

// tableName holds an upper's or raw layer's [idTable] in its root directory;
// the reserved prefix keeps it out of every index.
const tableName = metaPrefix + "ids"

// tableRecord is one record of an [idTable] log: inode number, then identity
// (0: none), both big-endian.
const tableRecord = 16

// idTable persists the identities of a directory's files that cannot hold an
// attribute (Linux symlinks and special files, or every file of a filesystem
// without extended attributes), by inode number. It is an append-only log,
// the last record of an inode winning, written before the file's identity is
// reported and before the file's last name is removed; a torn tail record is
// ignored. The log moves with the directory when an upper is frozen into a
// layer; a stored layer's manifest takes its place.
type idTable struct {
	mu      sync.Mutex
	path    string
	ids     map[uint64]ident.ID
	records int  // records in the log
	frozen  bool // a frozen layer's table, never written
}

// loadTable reads the table of directory root; a missing log is empty. A
// frozen table refuses writes.
func loadTable(root string, frozen bool) (*idTable, error) {
	t := &idTable{path: filepath.Join(root, tableName), ids: make(map[uint64]ident.ID), frozen: frozen}
	data, err := os.ReadFile(t.path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	for ; len(data) >= tableRecord; data = data[tableRecord:] {
		ino, id := binary.BigEndian.Uint64(data), ident.ID(binary.BigEndian.Uint64(data[8:]))
		if id == 0 {
			delete(t.ids, ino)
		} else {
			t.ids[ino] = id
		}
		t.records++
	}
	return t, nil
}

// get returns the identity recorded for inode ino; 0 when none is.
func (t *idTable) get(ino uint64) ident.ID {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ids[ino]
}

// set records id for inode ino, or forgets it with id 0, compacting the log
// once most of it is obsolete.
func (t *idTable) set(ino uint64, id ident.ID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.frozen {
		return &fs.PathError{Op: "write", Path: t.path, Err: fs.ErrPermission}
	}
	if cur, ok := t.ids[ino]; ok == (id != 0) && cur == id {
		return nil
	}
	if id == 0 {
		delete(t.ids, ino)
	} else {
		t.ids[ino] = id
	}
	if t.records >= 2*len(t.ids)+1024 {
		return t.rewrite()
	}
	var rec [tableRecord]byte
	binary.BigEndian.PutUint64(rec[:], ino)
	binary.BigEndian.PutUint64(rec[8:], uint64(id))
	f, err := os.OpenFile(t.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(rec[:])
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	t.records++
	return nil
}

// drop forgets inode ino.
func (t *idTable) drop(ino uint64) error { return t.set(ino, 0) }

// retain forgets every inode live does not hold, records that a crash
// between a file's removal and its record's left behind, and compacts the
// log. [FS.loadIndex] calls it with every inode of the upper.
func (t *idTable) retain(live map[uint64]bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.ids)
	for ino := range t.ids {
		if !live[ino] {
			delete(t.ids, ino)
		}
	}
	if len(t.ids) == n && t.records == n {
		return nil
	}
	return t.rewrite()
}

// empty reports whether the table records nothing.
func (t *idTable) empty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.ids) == 0
}

// rewrite replaces the log with one record per inode, or removes it when
// none is left. The caller holds t.mu.
func (t *idTable) rewrite() error {
	if len(t.ids) == 0 {
		if err := os.Remove(t.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		t.records = 0
		return nil
	}
	data := make([]byte, 0, len(t.ids)*tableRecord)
	for ino, id := range t.ids {
		data = binary.BigEndian.AppendUint64(data, ino)
		data = binary.BigEndian.AppendUint64(data, uint64(id))
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		os.Remove(tmp)
		return err
	}
	t.records = len(t.ids)
	return nil
}
