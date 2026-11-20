// Package durable flushes ovnfs's own metadata files (state.json, pins.log
// and the directories holding them) to stable storage.
//
// On macOS, (*os.File).Sync issues F_FULLFSYNC, which also drains the drive's
// write cache and costs tens of milliseconds per call. ovnfs's metadata must
// survive a crash of the server process or the kernel, not a power cut, so
// Sync uses plain fsync(2) there, as SQLite does by default. Other platforms
// keep (*os.File).Sync. Mount data syncs, including NFS stable writes and
// COMMIT, still use (*os.File).Sync for the backing file.
package durable
