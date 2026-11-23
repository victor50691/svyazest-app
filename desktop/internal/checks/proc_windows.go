//go:build windows

package checks

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// No console window; the core goes into a job object that kills it when
// the handle closes (also when the agent itself dies).
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
}

func attachKill(cmd *exec.Cmd) func(alive bool) {
	var job windows.Handle
	if j, err := windows.CreateJobObject(nil, nil); err == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(j, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err == nil {
			if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
				if windows.AssignProcessToJobObject(j, h) == nil {
					job = j
				}
				windows.CloseHandle(h)
			}
		}
		if job == 0 {
			windows.CloseHandle(j)
		}
	}
	return func(alive bool) {
		if alive {
			_ = cmd.Process.Kill()
		}
		if job != 0 {
			windows.CloseHandle(job) // KILL_ON_JOB_CLOSE
			job = 0
		}
	}
}
