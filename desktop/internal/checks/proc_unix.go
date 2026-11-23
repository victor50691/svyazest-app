//go:build !linux && !windows

package checks

import (
	"os/exec"
	"syscall"
)

func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func attachKill(cmd *exec.Cmd) func(alive bool) {
	pid := cmd.Process.Pid
	return func(alive bool) {
		if !alive {
			return // reaped: the group id may belong to someone else now
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
}
