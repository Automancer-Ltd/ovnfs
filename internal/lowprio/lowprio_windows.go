package lowprio

import "golang.org/x/sys/windows"

// threadModeBackgroundBegin is THREAD_MODE_BACKGROUND_BEGIN.
const threadModeBackgroundBegin = 0x00010000

var setThreadPriority = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetThreadPriority")

// Thread demotes the calling thread, which the caller's goroutine is locked to.
func Thread() error {
	if ok, _, err := setThreadPriority.Call(uintptr(windows.CurrentThread()), threadModeBackgroundBegin); ok == 0 {
		return err
	}
	return nil
}
