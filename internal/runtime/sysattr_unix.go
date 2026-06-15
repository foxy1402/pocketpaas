//go:build !linux && !windows

package runtime

import (
	"os/exec"
	"syscall"
)

// chrootEnabled is false on non-Linux platforms; startProcess uses host-path
// prefixing instead.
const chrootEnabled = false

func setSysProcAttr(cmd *exec.Cmd, _ string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
}
