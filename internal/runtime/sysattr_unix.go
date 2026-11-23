//go:build !linux && !windows

package runtime

import (
	"errors"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// chrootEnabled is false on non-Linux platforms; startProcess uses host-path
// prefixing instead.
const chrootEnabled = false

// usernsChrootEnabled is never set outside Linux; prefix mode is used.
var usernsChrootEnabled atomic.Bool

func setSysProcAttr(cmd *exec.Cmd, _ string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
}

func killProcessGroupForce(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// InitIsolation is a no-op off Linux: prefix mode is the only option.
func InitIsolation() {}

// RunUsernsProbe is unsupported off Linux.
func RunUsernsProbe(dir string) error {
	return errors.New("virtual root (user namespaces) is only supported on Linux")
}

// IsolationMode is always prefix mode off Linux.
func IsolationMode() string { return "prefix" }

// DisableUsernsChroot is a no-op off Linux.
func DisableUsernsChroot(reason error) {}

func isEPERM(err error) bool {
	return errors.Is(err, syscall.EPERM)
}
