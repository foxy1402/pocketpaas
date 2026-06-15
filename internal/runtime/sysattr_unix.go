//go:build !windows

package runtime

import (
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup sends SIGTERM to the entire process group.
func killProcessGroup(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
}
