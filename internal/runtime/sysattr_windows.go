//go:build windows

package runtime

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {}
