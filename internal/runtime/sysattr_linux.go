//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// chrootEnabled is set at startup by probing whether CAP_SYS_CHROOT is
// available. It is true when pocketpaas is running as the container's main
// process (e.g. via the ghcr Docker image), and false when running inside a
// restricted PaaS-assigned container reached only by SSH.
var chrootEnabled bool

// usernsChrootEnabled tracks whether unprivileged user namespaces allow a
// chroot — the "virtual root" fallback when CAP_SYS_CHROOT is absent. Atomic
// because the runtime self-heal (EPERM at spawn time) can flip it off after
// boot if the environment's policy tightened.
var usernsChrootEnabled atomic.Bool

// InitIsolation probes the strongest available isolation mode once at
// startup: direct chroot → virtual root (unprivileged user+mount namespace)
// → rootfs-prefix mode. Call it from main(), never from init(): the
// virtual-root probe re-executes this binary, and an init-time ladder would
// run again in the probe child, spawning probe grandchildren recursively
// until the timeout killed them.
func InitIsolation() {
	// chroot("/") is a no-op if it succeeds (root stays the same) but
	// requires CAP_SYS_CHROOT. EPERM means the capability is absent.
	if err := syscall.Chroot("/"); err == nil {
		chrootEnabled = true
		log.Printf("runtime: chroot(2) available — apps run in direct chroot mode")
		return
	} else {
		log.Printf("runtime: chroot(2) unavailable (%v) — probing unprivileged user namespaces (virtual root)", err)
	}
	if probeUsernsChroot() {
		usernsChrootEnabled.Store(true)
		log.Printf("runtime: user namespaces available — apps run in virtual-root mode (chroot inside a private user+mount namespace, uid 0 emulated)")
		return
	}
	log.Printf("runtime: user namespaces unavailable — apps will run in rootfs-prefix mode (no filesystem isolation)")
	log.Printf("runtime: hint: check sysctl kernel.unprivileged_userns_clone / user.max_user_namespaces; Ubuntu 24.04+ AppArmor: kernel.apparmor_restrict_unprivileged_userns=0; Docker's default seccomp profile denies clone(CLONE_NEWUSER) for non-root users (--security-opt seccomp=unconfined lifts it)")
	log.Printf("runtime: prefix mode only execs binaries that are static or whose ELF interpreter exists on the host — e.g. a glibc image on a musl host will fail to start its binaries")
}

// probeUsernsChroot re-executes this binary with --internal-userns-probe,
// using the exact mechanism app starts use (see setSysProcAttr): clone with
// CLONE_NEWUSER|CLONE_NEWNS plus single-entry uid/gid maps written by the
// parent. The child then chroots itself as the namespace root. Success proves
// apps can get real chroot isolation plus an emulated uid 0 with no host
// capabilities.
//
// In-process unshare(CLONE_NEWUSER) is NOT usable as a probe: the Go runtime
// is multithreaded and the kernel rejects that with EINVAL (verified).
func probeUsernsChroot() bool {
	dir, err := os.MkdirTemp("", "pp-userns-probe-*")
	if err != nil {
		log.Printf("runtime: virtual-root probe skipped: %v", err)
		return false
	}
	defer os.RemoveAll(dir)
	exe, err := os.Executable()
	if err != nil {
		log.Printf("runtime: virtual-root probe skipped: %v", err)
		return false
	}
	// 5 s safety net only — clone either succeeds immediately or returns EPERM.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--internal-userns-probe", dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getegid(), Size: 1},
		},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("runtime: virtual-root probe failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}

// RunUsernsProbe is the probe child entrypoint (cmd/server handles the
// --internal-userns-probe flag before anything else). It already runs inside
// the user+mount namespace created by probeUsernsChroot — Go's clone path
// wrote the uid/gid maps from the parent — so it is uid 0 here with full
// capabilities in that namespace. Chrooting proves the exact op app starts
// depend on.
func RunUsernsProbe(dir string) error {
	if uid := os.Getuid(); uid != 0 {
		return fmt.Errorf("expected uid 0 inside the new user namespace, got %d", uid)
	}
	if err := syscall.Chroot(dir); err != nil {
		return fmt.Errorf("chroot as namespace root: %w", err)
	}
	fmt.Fprintln(os.Stderr, "probe-ok: uid 0 in user+mount namespace, chroot(2) succeeded")
	return nil
}

// IsolationMode reports the strongest subprocess isolation available:
// "chroot" (real root / CAP_SYS_CHROOT), "virtual-root" (unprivileged user
// namespace + chroot), or "prefix" (rootfs path prefixing, no isolation).
func IsolationMode() string {
	if chrootEnabled {
		return "chroot"
	}
	if usernsChrootEnabled.Load() {
		return "virtual-root"
	}
	return "prefix"
}

// DisableUsernsChroot is called by the runtime self-heal when a spawn fails
// with EPERM while in virtual-root mode: the environment allows the boot
// probe but denies namespace creation at exec time (seccomp/AppArmor), so
// all future starts fall back to rootfs-prefix mode.
func DisableUsernsChroot(reason error) {
	if usernsChrootEnabled.CompareAndSwap(true, false) {
		log.Printf("runtime: virtual-root mode disabled after spawn failure (%v) — falling back to rootfs-prefix mode", reason)
	}
}

// isEPERM reports whether err is (or wraps) EPERM — the signature of a
// policy denying clone/unshare. Defined per-GOOS: windows has no errno.
func isEPERM(err error) bool {
	return errors.Is(err, syscall.EPERM)
}

// setSysProcAttr sets process-group isolation and, when rootfsPath is
// non-empty, chroots the child — directly when we have CAP_SYS_CHROOT, or
// inside a fresh user+mount namespace ("virtual root") when we don't:
// the mapped root has CAP_SYS_CHROOT in its own namespace, so chroot(2)
// succeeds unprivileged. Go's parent writes the single-entry uid/gid maps
// while the child is suspended (setgroups denied automatically).
func setSysProcAttr(cmd *exec.Cmd, rootfsPath string) {
	a := &syscall.SysProcAttr{Setpgid: true}
	if rootfsPath != "" {
		if chrootEnabled {
			a.Chroot = rootfsPath
		} else if usernsChrootEnabled.Load() {
			a.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS
			a.UidMappings = []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: syscall.Getuid(), Size: 1},
			}
			a.GidMappings = []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: syscall.Getegid(), Size: 1},
			}
			a.Chroot = rootfsPath
		}
	}
	cmd.SysProcAttr = a
}

// terminateProcessGroup sends SIGTERM to the entire process group.
func terminateProcessGroup(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGTERM)
	}
}

// killProcessGroupForce sends SIGKILL to the entire process group — used when
// the SIGTERM grace period expires so stubborn processes actually die.
func killProcessGroupForce(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}
