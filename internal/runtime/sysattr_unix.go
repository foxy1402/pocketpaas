//go:build !linux && !windows

package runtime

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync/atomic"
	"syscall"
)

// chrootEnabled is never set on non-Linux platforms; startProcess uses
// host-path prefixing instead.
var chrootEnabled atomic.Bool

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

// DisableChroot is a no-op off Linux.
func DisableChroot(reason error) {}

// ProbeVirtualRoot is unsupported off Linux.
func ProbeVirtualRoot() bool { return false }

// Demote is a no-op off Linux: prefix mode is the end of the ladder.
func Demote(reason error) bool { return false }

// SpawnDiagnostics is Linux-only (no /proc or capabilities to report here).
func SpawnDiagnostics(rootfs string) string { return "" }

// Diagnose reports that isolation diagnostics are Linux-only.
func Diagnose(w io.Writer) {
	fmt.Fprintln(w, "isolation diagnostics are only available on Linux")
}

func isEPERM(err error) bool {
	return errors.Is(err, syscall.EPERM)
}
