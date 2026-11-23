//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// chrootEnabled tracks whether CAP_SYS_CHROOT is available. Atomic because a
// spawn failure can demote the mode at runtime (the boot probe only proves the
// probe process may chroot — not that every spawned child may).
var chrootEnabled atomic.Bool

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
		chrootEnabled.Store(true)
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
	if chrootEnabled.Load() {
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

// DisableChroot demotes direct-chroot mode after a spawn was denied. The boot
// probe only proves the probe process may chroot; exec-time policy can still
// deny the child's clone/chroot/execve, and then the lower tiers are tried.
func DisableChroot(reason error) {
	if chrootEnabled.CompareAndSwap(true, false) {
		log.Printf("runtime: direct chroot mode disabled after spawn failure (%v) — probing lower tiers", reason)
	}
}

// ProbeVirtualRoot runs the user-namespace probe on demand (after a chroot
// spawn failure the boot probe was skipped because chroot succeeded) and
// enables virtual-root mode when it succeeds. Reports whether virtual-root
// mode is now active.
func ProbeVirtualRoot() bool {
	if chrootEnabled.Load() {
		return false
	}
	if usernsChrootEnabled.Load() {
		return true
	}
	if !probeUsernsChroot() {
		return false
	}
	usernsChrootEnabled.Store(true)
	log.Printf("runtime: user namespaces available after demotion — apps run in virtual-root mode")
	return true
}

// SpawnDiagnostics summarises the security context that decides whether a
// spawned app child may clone/chroot/execve: identity, effective capability
// mask, no_new_privs, seccomp mode, and the rootfs mount flags. It is logged
// when a start fails with EPERM so the denial can be attributed without
// guesswork.
func SpawnDiagnostics(rootfs string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "uid=%d euid=%d gid=%d egid=%d", os.Getuid(), os.Geteuid(), os.Getgid(), os.Getegid())
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "CapEff:"),
				strings.HasPrefix(line, "NoNewPrivs:"),
				strings.HasPrefix(line, "Seccomp:"):
				b.WriteString(" ")
				b.WriteString(strings.TrimSpace(line))
			}
		}
	}
	if rootfs != "" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(rootfs, &st); err == nil {
			flags := uint64(st.Flags)
			fmt.Fprintf(&b, " rootfs-mount-flags=0x%x(noexec=%t nosuid=%t ro=%t)",
				flags, flags&syscall.MS_NOEXEC != 0, flags&syscall.MS_NOSUID != 0, flags&syscall.MS_RDONLY != 0)
		}
	}
	return b.String()
}

// Diagnose writes a support report about this environment's isolation
// capabilities (hidden --diagnose flag). Every probe mirrors an operation app
// starts actually perform, so a denied syscall is visible here instead of
// surfacing only as "fork/exec …: operation not permitted".
func Diagnose(w io.Writer) {
	fmt.Fprintln(w, "pocketpaas isolation diagnostics")
	fmt.Fprintln(w, "environment:", SpawnDiagnostics(""))

	if err := syscall.Chroot("/"); err != nil {
		fmt.Fprintf(w, "probe: chroot(/): %v\n", err)
	} else {
		fmt.Fprintln(w, "probe: chroot(/): ok (CAP_SYS_CHROOT available)")
	}

	// Plain spawn: clone(CLONE_VFORK|CLONE_VM|SIGCHLD) + execve.
	spawn := exec.Command("/bin/sh", "-c", "exit 0")
	if err := spawn.Run(); err != nil {
		fmt.Fprintf(w, "probe: spawn /bin/sh: %v\n", err)
	} else {
		fmt.Fprintln(w, "probe: spawn /bin/sh: ok")
	}

	// Chrooted spawn: exactly what an app start does in chroot mode.
	if dir, err := os.MkdirTemp("", "pp-diag-*"); err == nil {
		defer os.RemoveAll(dir)
		if err := copyExecutable(dir); err != nil {
			fmt.Fprintf(w, "probe: chrooted spawn: setup failed: %v\n", err)
		} else {
			cmd := exec.Command("/pp-diag", "--internal-diag-child")
			cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: dir}
			if err := cmd.Run(); err != nil {
				fmt.Fprintf(w, "probe: chrooted spawn: %v\n", err)
			} else {
				fmt.Fprintln(w, "probe: chrooted spawn: ok")
			}
		}
	}

	if probeUsernsChroot() {
		fmt.Fprintln(w, "probe: virtual root (clone NEWUSER|NEWNS + chroot): ok")
		fmt.Fprintln(w, "fallback: virtual-root")
	} else {
		fmt.Fprintln(w, "probe: virtual root (clone NEWUSER|NEWNS + chroot): failed")
		fmt.Fprintln(w, "fallback: prefix (no filesystem isolation)")
	}
}

// copyExecutable places a copy of this binary at dir/pp-diag so a chrooted
// child can exec it (exec paths resolve inside the chroot).
func copyExecutable(dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, "pp-diag"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
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
		if chrootEnabled.Load() {
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
