// Package dataless lets ovnfs read cloud-only files on macOS.
//
// File Provider folders (iCloud Drive with Optimize Mac Storage, synced
// Desktop & Documents, Google Drive streaming, Dropbox online-only) keep
// evicted files as dataless placeholders whose bytes download on first read.
// A per-process I/O policy, inherited by every process started afterwards,
// decides whether that read downloads or fails at once with EDEADLK
// ("resource deadlock avoided"). The system default is off: a terminal started
// as a launchd job without MaterializeDatalessFiles hands the off policy to
// its shells, and through them to ovnfs and the daemon it starts, which then
// fails every read of a checkout's evicted file.
//
//	func main() {
//		_ = dataless.Materialize() // best effort: such reads fail as before without it
//		…
//	}
package dataless
