//go:build windows

package runtime

import "os/exec"

const chrootEnabled = false

func setSysProcAttr(_ *exec.Cmd, _ string) {}
func killProcessGroup(_ int)               {}
