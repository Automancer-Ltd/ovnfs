//go:build darwin

package dataless

import (
	"syscall"
	_ "unsafe" // for go:linkname and go:cgo_import_dynamic
)

// <sys/resource.h> setiopolicy_np arguments.
const (
	iopolTypeVFSMaterializeDatalessFiles = 3 // IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES
	iopolScopeProcess                    = 0 // IOPOL_SCOPE_PROCESS
	iopolMaterializeDatalessFilesOn      = 2 // IOPOL_MATERIALIZE_DATALESS_FILES_ON
)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

var libc_setiopolicy_np_trampoline_addr uintptr

//go:cgo_import_dynamic libc_setiopolicy_np setiopolicy_np "/usr/lib/libSystem.B.dylib"

// Materialize has this process, and the processes it starts from now on,
// download a dataless file when they read it, as programs in Terminal.app do,
// instead of failing the read with EDEADLK. It returns setiopolicy_np's errno
// when the kernel refuses the policy.
func Materialize() error {
	_, _, e := syscall_syscall6(libc_setiopolicy_np_trampoline_addr,
		iopolTypeVFSMaterializeDatalessFiles, iopolScopeProcess, iopolMaterializeDatalessFilesOn, 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
