//go:build linux

package checks

import (
	"os/exec"
	"syscall"
)

// Own process group (killed as a whole) and SIGKILL if the agent dies.
func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
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
