//go:build linux

package runtime

import (
	"os/exec"
	"syscall"
)

// chrootEnabled tells startProcess that SysProcAttr.Chroot is available so it
// can use proper chroot semantics instead of host-path prefixing.
const chrootEnabled = true

// setSysProcAttr sets process-group isolation (so all children die on stop) and
// optionally chroots the child into rootfsPath before exec.
// An empty rootfsPath means no chroot.
func setSysProcAttr(cmd *exec.Cmd, rootfsPath string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Chroot:  rootfsPath,
	}
}

// killProcessGroup sends SIGTERM to the entire process group.
func killProcessGroup(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
}
