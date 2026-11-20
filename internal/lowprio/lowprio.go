// Package lowprio demotes a thread to the lowest scheduling and I/O priority
// the OS offers, for housekeeping that may take as long as it likes: deleting
// a tree of hundreds of thousands of files (a frozen layer holding a build's
// output) is one unlink each, and at normal priority it competes with the
// builds and file calls the daemon serves.
//
//	Platform  Primitive                                  Effect
//	macOS     setpriority(PRIO_DARWIN_THREAD, BG)        background QoS: efficiency cores, throttled I/O
//	Linux     setpriority(nice 19), ioprio_set(idle)     runs and reads the disk when nothing else wants to
//	Windows   SetThreadPriority(BACKGROUND_BEGIN)        lowest CPU, I/O and memory priority
//
// A priority belongs to an OS thread, not a goroutine, so the caller locks its
// goroutine to one first and never unlocks it: the demoted thread then ends
// with the goroutine instead of returning to the scheduler's pool.
//
//	go func() {
//		runtime.LockOSThread()
//		lowprio.Thread() // best effort: the work is merely less polite without it
//		deleteEverything()
//	}()
package lowprio
