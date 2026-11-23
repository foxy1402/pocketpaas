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

// ProotAvailable reports whether the proot binary is installed.
func ProotAvailable() bool {
	_, err := os.Stat(prootBin)
	return err == nil
}

// InitIsolation probes the strongest available isolation mode once at
// startup: direct chroot → virtual root (unprivileged user+mount namespace)
// → proot → rootfs-prefix mode. Call it from main(), never from init(): the
// probes re-execute this binary, and an init-time ladder would run again in
// the probe child, spawning probe grandchildren recursively until the timeout
// killed them.
func InitIsolation() {
	// chroot("/") requires CAP_SYS_CHROOT, so EPERM rules chroot mode out
	// cheaply. Success is NOT sufficient though: a sandbox can allow chroot(2)
	// and still deny spawning a child that chroots (observed on managed PaaS
	// runtimes), so the real test is a chrooted spawn.
	if err := syscall.Chroot("/"); err != nil {
		log.Printf("runtime: chroot(2) unavailable (%v) — probing unprivileged user namespaces (virtual root)", err)
	} else if probeChrootSpawn() {
		chrootEnabled.Store(true)
		log.Printf("runtime: chroot(2) available — apps run in direct chroot mode")
		return
	} else {
		log.Printf("runtime: chroot(2) is permitted but spawning a chrooted child was denied — probing lower tiers")
	}
	if probeUsernsChroot() {
		usernsChrootEnabled.Store(true)
		log.Printf("runtime: user namespaces available — apps run in virtual-root mode (chroot inside a private user+mount namespace, uid 0 emulated)")
		return
	}
	if probeProot() {
		prootEnabled.Store(true)
		log.Printf("runtime: no chroot and no user namespaces — apps run in %s", prootModeDesc())
		return
	}
	log.Printf("runtime: proot unavailable — apps will run in rootfs-prefix mode (no filesystem isolation)")
	log.Printf("runtime: hint: check sysctl kernel.unprivileged_userns_clone / user.max_user_namespaces; Ubuntu 24.04+ AppArmor: kernel.apparmor_restrict_unprivileged_userns=0; Docker's default seccomp profile denies clone(CLONE_NEWUSER) for non-root users (--security-opt seccomp=unconfined lifts it)")
	log.Printf("runtime: prefix mode only execs binaries that are static or whose ELF interpreter exists on the host — e.g. a glibc image on a musl host will fail to start its binaries")
}

// probeChrootSpawn performs the exact operation an app start in chroot mode
// does — clone a child, chroot it into a directory, exec a binary from inside
// that directory — using a copy of this binary as the payload. chroot("/")
// alone is not a valid probe: it only proves CAP_SYS_CHROOT, while sandboxes
// can deny the child's chroot or its execve.
func probeChrootSpawn() bool {
	dir, err := os.MkdirTemp("", "pp-chroot-probe-*")
	if err != nil {
		log.Printf("runtime: chroot probe skipped: %v", err)
		return false
	}
	defer os.RemoveAll(dir)
	if err := copyExecutable(dir); err != nil {
		log.Printf("runtime: chroot probe skipped: %v", err)
		return false
	}
	// 5 s safety net: a policy denial returns immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// exec.Command only calls LookPath for bare names, so "/pp-diag" is passed
	// through untouched and the kernel resolves it after the child chroots
	// (the host has no /pp-diag).
	cmd := exec.CommandContext(ctx, "/pp-diag", "--internal-diag-child")
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: dir}
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("runtime: chrooted spawn failed: %v (%s)", err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}

// probeProot checks that proot is installed and that this sandbox lets it
// trace a child — ptrace permission is the one right a seccomp/LSM policy can
// still take away, and without it proot cannot operate at all.
//
// proot's default accelerator uses ptrace's seccomp events
// (PTRACE_O_TRACESECCOMP). Syscall-emulation sandboxes accept ptrace but reject
// that option ("ptrace option PTRACE_O_TRACESECCOMP not supported yet", exit
// 255), so a failed accelerated probe is retried with PROOT_NO_SECCOMP=1,
// which falls back to classic syscall-stop tracing.
func probeProot() bool {
	if !ProotAvailable() {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("runtime: proot probe skipped: %v", err)
		return false
	}
	// Operator override / manual escape hatch: probe classic mode only.
	if os.Getenv("PROOT_NO_SECCOMP") != "" {
		if runProotProbe(exe, true) {
			prootNoSeccomp.Store(true)
			return true
		}
		return false
	}
	if runProotProbe(exe, false) {
		return true
	}
	log.Printf("runtime: proot seccomp acceleration unavailable — retrying in classic ptrace mode (PROOT_NO_SECCOMP=1)")
	if runProotProbe(exe, true) {
		prootNoSeccomp.Store(true)
		return true
	}
	return false
}

// runProotProbe traces one exec of this binary through proot (-r / keeps path
// translation an identity mapping; the tracer machinery is what is under test).
func runProotProbe(exe string, classic bool) bool {
	// 20 s: classic mode is noticeably slower to start, and a denied syscall
	// returns immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, prootBin, "-v", "-1", "-r", "/", exe, "--internal-diag-child")
	if classic {
		cmd.Env = append(os.Environ(), "PROOT_NO_SECCOMP=1")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		mode := "seccomp-accelerated"
		if classic {
			mode = "classic"
		}
		log.Printf("runtime: proot probe (%s mode) failed: %v (%s)", mode, err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}

// EnableProot probes and enables proot mode. Reports whether the tier is (now)
// active.
func EnableProot() bool {
	if prootEnabled.Load() {
		return true
	}
	if !probeProot() {
		return false
	}
	prootEnabled.Store(true)
	log.Printf("runtime: proot available — apps run in %s", prootModeDesc())
	return true
}

// DisableProot demotes past proot, leaving plain rootfs-prefix mode.
func DisableProot(reason error) {
	if prootEnabled.CompareAndSwap(true, false) {
		log.Printf("runtime: proot mode disabled after spawn failure (%v) — falling back to rootfs-prefix mode", reason)
	}
}

// Demote moves one tier down the isolation ladder after an app spawn was
// denied, so Manager.Start can retry the start. Reports whether the active
// mode changed; false means the ladder is exhausted (prefix mode) and the
// start error should be surfaced as-is.
func Demote(reason error) bool {
	before := IsolationMode()
	switch before {
	case "prefix":
		return false
	case "proot":
		DisableProot(reason)
	case "virtual-root":
		DisableUsernsChroot(reason)
	case "chroot":
		DisableChroot(reason)
		ProbeVirtualRoot()
	}
	// Everything above prefix failed: try proot before giving up on a rootfs view.
	if IsolationMode() == "prefix" {
		EnableProot()
	}
	return IsolationMode() != before
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
// namespace + chroot), "proot" (ptrace path translation, fake uid 0), or
// "prefix" (rootfs path prefixing, no isolation).
func IsolationMode() string {
	if chrootEnabled.Load() {
		return "chroot"
	}
	if usernsChrootEnabled.Load() {
		return "virtual-root"
	}
	if prootEnabled.Load() {
		return "proot"
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

	// Capability check only — success does not prove a chrooted child can spawn.
	if err := syscall.Chroot("/"); err != nil {
		fmt.Fprintf(w, "probe: chroot(2) capability: %v\n", err)
	} else {
		fmt.Fprintln(w, "probe: chroot(2) capability: ok (CAP_SYS_CHROOT present)")
	}

	// Plain spawn: exactly what prefix mode does.
	spawn := exec.Command("/bin/sh", "-c", "exit 0")
	if err := spawn.Run(); err != nil {
		fmt.Fprintf(w, "probe: spawn /bin/sh: %v\n", err)
	} else {
		fmt.Fprintln(w, "probe: spawn /bin/sh: ok")
	}

	// Chrooted spawn: exactly what an app start does in chroot mode.
	chrootOK := probeChrootSpawn()
	if chrootOK {
		fmt.Fprintln(w, "probe: chrooted spawn (clone + chroot + execve): ok")
	} else {
		fmt.Fprintln(w, "probe: chrooted spawn (clone + chroot + execve): failed")
	}

	usernsOK := probeUsernsChroot()
	if usernsOK {
		fmt.Fprintln(w, "probe: virtual root (clone NEWUSER|NEWNS + chroot): ok")
	} else {
		fmt.Fprintln(w, "probe: virtual root (clone NEWUSER|NEWNS + chroot): failed")
	}

	prootOK := false
	switch {
	case !ProotAvailable():
		fmt.Fprintln(w, "probe: proot: not installed")
	case probeProot():
		prootOK = true
		fmt.Fprintf(w, "probe: %s: ok\n", prootModeDesc())
	default:
		fmt.Fprintln(w, "probe: proot (ptrace path translation, fake uid 0): failed — ptrace denied")
	}

	switch {
	case chrootOK:
		fmt.Fprintln(w, "expected isolation: chroot")
	case usernsOK:
		fmt.Fprintln(w, "expected isolation: virtual-root")
	case prootOK:
		fmt.Fprintln(w, "expected isolation: proot")
	default:
		fmt.Fprintln(w, "expected isolation: prefix (no filesystem isolation — only static binaries or images whose ELF interpreter exists on the host will start)")
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
// non-empty, chroots the child. The caller's chroot-mode snapshot is honoured
// as-is: direct chroot is signalled by usernsChrootEnabled being false while
// rootfsPath is non-empty (the direct case), because the cmd was built with
// container-abs host paths that only resolve once chrooted. Inside a fresh
// user+mount namespace ("virtual root") the mapped root has CAP_SYS_CHROOT in
// its own namespace, so chroot(2) succeeds unprivileged; Go's parent writes
// the single-entry uid/gid maps while the child is suspended (setgroups denied
// automatically).
//
// rootfsPath is the snapshot the caller already used to build the command
// (container-abs paths, workdir); it is not re-derived here, so the attrs
// always agree with the rest of the spawn even if another app's start demotes
// the ladder between the snapshot and this call.
func setSysProcAttr(cmd *exec.Cmd, rootfsPath string) {
	a := &syscall.SysProcAttr{Setpgid: true}
	if rootfsPath != "" {
		if usernsChrootEnabled.Load() {
			a.Cloneflags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS
			a.UidMappings = []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: syscall.Getuid(), Size: 1},
			}
			a.GidMappings = []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: syscall.Getegid(), Size: 1},
			}
		}
		a.Chroot = rootfsPath
	}
	cmd.SysProcAttr = a
}

// signalsSupported reports whether the platform can signal a process group.
const signalsSupported = true

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
