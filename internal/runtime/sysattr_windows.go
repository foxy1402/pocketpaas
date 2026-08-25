//go:build windows

package runtime

import "os/exec"

const chrootEnabled = false

func setSysProcAttr(_ *exec.Cmd, _ string) {}

// Windows has no POSIX process groups for us to signal. ProcessTree teardown
// relies on exec.CommandContext's Cancel killing the leader process;
// grandchildren (e.g. npm -> node) may be orphaned. Windows is a dev-only
// platform for this tool, so this limitation is acceptable.
func terminateProcessGroup(_ int) {}
func killProcessGroupForce(_ int) {}
