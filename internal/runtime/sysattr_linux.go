//go:build linux

package runtime

import (
	"log"
	"os/exec"
	"syscall"
)

// chrootEnabled is set at startup by probing whether CAP_SYS_CHROOT is
// available. It is true when pocketpaas is running as the container's main
// process (e.g. via the ghcr Docker image), and false when running inside a
// restricted PaaS-assigned container reached only by SSH.
var chrootEnabled bool

func init() {
	// chroot("/") is a no-op if it succeeds (root stays the same) but
	// requires CAP_SYS_CHROOT. EPERM means the capability is absent.
	if err := syscall.Chroot("/"); err == nil {
		chrootEnabled = true
	} else {
		log.Printf("runtime: chroot(2) unavailable (%v) — apps will run in rootfs-prefix mode (no filesystem isolation)", err)
		log.Printf("runtime: this is normal inside SSH-only PaaS containers; apps still work via LD_LIBRARY_PATH resolution")
	}
}

// setSysProcAttr sets process-group isolation (so all children die on stop)
// and, when rootfsPath is non-empty, chroots the child into that directory.
func setSysProcAttr(cmd *exec.Cmd, rootfsPath string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Chroot:  rootfsPath, // empty string = no chroot
	}
}

// killProcessGroup sends SIGTERM to the entire process group.
func killProcessGroup(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
}
