//go:build windows

package runtime

import (
	"errors"
	"os/exec"
	"sync/atomic"
)

const chrootEnabled = false

// usernsChrootEnabled is never set on Windows; prefix mode is used.
var usernsChrootEnabled atomic.Bool

func setSysProcAttr(_ *exec.Cmd, _ string) {}

// Windows has no POSIX process groups for us to signal. ProcessTree teardown
// relies on exec.CommandContext's Cancel killing the leader process;
// grandchildren (e.g. npm -> node) may be orphaned. Windows is a dev-only
// platform for this tool, so this limitation is acceptable.
func terminateProcessGroup(_ int) {}
func killProcessGroupForce(_ int) {}

// InitIsolation is a no-op on Windows: prefix mode is the only option.
func InitIsolation() {}

// RunUsernsProbe is unsupported on Windows.
func RunUsernsProbe(dir string) error {
	return errors.New("virtual root (user namespaces) is only supported on Linux")
}

// IsolationMode is always prefix mode on Windows.
func IsolationMode() string { return "prefix" }

// DisableUsernsChroot is a no-op on Windows.
func DisableUsernsChroot(reason error) {}

func isEPERM(err error) bool { return false }
