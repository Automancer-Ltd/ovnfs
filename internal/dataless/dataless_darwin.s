// libSystem trampoline for setiopolicy_np (amd64 and arm64 share this
// syntax), invoked through syscall.syscall6 so the runtime handles errno and
// scheduler hand-off.

#include "textflag.h"

TEXT libc_setiopolicy_np_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_setiopolicy_np(SB)
GLOBL	·libc_setiopolicy_np_trampoline_addr(SB), RODATA, $8
DATA	·libc_setiopolicy_np_trampoline_addr(SB)/8, $libc_setiopolicy_np_trampoline<>(SB)
