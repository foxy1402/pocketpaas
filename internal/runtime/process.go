package runtime

import (
	"bufio"
	"context"
	"debug/elf"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync/atomic"
	"time"

	"apphive/internal/store"
)

// Process wraps a running os/exec subprocess.
type Process struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	logs     *LogBuffer
	exited   chan struct{} // closed by wait() once the OS process has exited
	done     chan struct{} // closed by the manager AFTER removing it from the process registry
	stopping int32         // atomic; 1 = intentional stop (don't mark as crashed)
}

// startProcess launches a subprocess for the given app.
// The caller must hold the app's per-app operation lock while calling this.
func startProcess(app *store.App, logs *LogBuffer) (*Process, error) {
	if len(app.Entrypoint) == 0 && len(app.Command) == 0 {
		return nil, fmt.Errorf("no entrypoint or command configured — pull the image first or set an override")
	}

	// Copy defensively: append could otherwise write into the store's shared
	// entrypoint backing array if it has spare capacity.
	rawArgv := append(append([]string{}, app.Entrypoint...), app.Command...)
	// Isolation ladder: direct chroot when we have CAP_SYS_CHROOT, otherwise
	// "virtual root" (chroot inside a private user+mount namespace, emulated
	// uid 0) when unprivileged user namespaces work, else rootfs-prefix mode.
	useChroot := app.RootfsPath != "" && (chrootEnabled || usernsChrootEnabled.Load())

	ctx, cancel := context.WithCancel(context.Background())

	var cmd *exec.Cmd

	if useChroot {
		// ── Chroot path (Linux with CAP_SYS_CHROOT) ──────────────────────────
		// Resolve the binary within the extracted rootfs.
		// We build exec.Cmd directly (bypassing exec.Command) so that
		// LookPath is not called on the HOST; the kernel resolves the path
		// relative to the new root after chroot().
		bin := lookPathInRootfs(rawArgv[0], app.RootfsPath)
		cmd = &exec.Cmd{
			Path: bin,
			Args: rawArgv, // argv[0] is the program name the process sees
		}
		if app.WorkDir != "" {
			cmd.Dir = app.WorkDir // interpreted relative to the chroot root
		} else {
			cmd.Dir = "/"
		}
		// Copy DNS files so the app can reach the network inside the chroot.
		ensureChrootDNS(app.RootfsPath)
	} else {
		// ── Non-chroot / rootfs-prefix path ──────────────────────────────────
		// Used on non-Linux platforms (dev builds) and on Linux containers
		// that lack CAP_SYS_CHROOT (SSH-only PaaS containers).
		//
		// Step 1: prefix every container-absolute argv token with the rootfs
		// path so the kernel can find and exec the binary on the host
		// filesystem. (isContainerAbs, not filepath.IsAbs: on Windows
		// "/usr/bin/node" is not an absolute host path.)
		argv := make([]string, len(rawArgv))
		for i, a := range rawArgv {
			if app.RootfsPath != "" && isContainerAbs(a) {
				prefixed, err := joinUnderRoot(app.RootfsPath, a)
				if err != nil {
					cancel()
					return nil, fmt.Errorf("invalid path in args: %w", err)
				}
				argv[i] = prefixed
			} else {
				argv[i] = a
			}
		}
		// If argv[0] is a bare name (e.g. "node", "python3") resolve it
		// inside the rootfs — otherwise exec.CommandContext would search
		// the HOST PATH and exec the wrong (or no) binary.
		if app.RootfsPath != "" && !isContainerAbs(argv[0]) {
			resolved := lookPathInRootfs(argv[0], app.RootfsPath)
			if !isContainerAbs(resolved) {
				cancel()
				return nil, fmt.Errorf("command %q not found in image rootfs (searched standard bin directories) — refusing to fall back to the host PATH", rawArgv[0])
			}
			prefixed, err := joinUnderRoot(app.RootfsPath, resolved)
			if err != nil {
				cancel()
				return nil, err
			}
			argv[0] = prefixed
		}
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)

		if app.RootfsPath != "" {
			if app.WorkDir != "" {
				dir, err := joinUnderRoot(app.RootfsPath, app.WorkDir)
				if err != nil {
					cancel()
					return nil, fmt.Errorf("invalid work_dir: %w", err)
				}
				cmd.Dir = dir
			} else {
				cmd.Dir = app.RootfsPath
			}
		} else if app.WorkDir != "" {
			cmd.Dir = app.WorkDir
		}

		// Step 2: use the image's own dynamic linker so the binary runs
		// correctly regardless of the host's libc flavour.
		//
		// Every dynamically-linked ELF binary has a PT_INTERP segment that
		// names its required dynamic linker (e.g. /lib/ld-musl-x86_64.so.1
		// for Alpine, or /lib/x86_64-linux-gnu/ld-linux-x86_64.so.2 for
		// Debian). If we exec the binary directly on a host with a different
		// libc, the kernel will fail to find that interpreter.
		//
		// Solution: read PT_INTERP from the binary, locate that linker inside
		// the rootfs, and invoke it as:
		//   <rootfs-linker> --library-path <rootfs-lib-dirs> <binary> [args…]
		//
		// This works for any image+host combination — Alpine on Debian,
		// Debian on Alpine, etc. — with zero capabilities required.
		// Static binaries (Go, Rust/musl) have no PT_INTERP; they fall
		// through and exec correctly as-is.
		if app.RootfsPath != "" {
			interp := readELFInterpreter(cmd.Path)
			if interp != "" {
				interpHost, jerr := joinUnderRoot(app.RootfsPath, interp)
				if jerr != nil {
					cancel()
					return nil, fmt.Errorf("invalid ELF interpreter path: %w", jerr)
				}
				if _, err := os.Stat(interpHost); err == nil {
					libDirs := findRootfsLibDirs(app.RootfsPath)
					newArgv := []string{interpHost}
					if len(libDirs) > 0 {
						newArgv = append(newArgv, "--library-path", strings.Join(libDirs, ":"))
					}
					newArgv = append(newArgv, argv...)
					cmd.Path = interpHost
					cmd.Args = newArgv
				}
			} else if shell, arg := readShebang(cmd.Path); shell != "" {
				shellHost, jerr := joinUnderRoot(app.RootfsPath, shell)
				if jerr != nil {
					cancel()
					return nil, fmt.Errorf("invalid shebang interpreter path: %w", jerr)
				}
				if _, err := os.Stat(shellHost); err == nil {
					newArgv := []string{shellHost}
					if arg != "" {
						newArgv = append(newArgv, arg)
					}
					newArgv = append(newArgv, argv...)

					// The shebang-resolved interpreter (e.g. /bin/sh) is
					// typically a dynamically-linked ELF binary. It needs the
					// same rootfs ld.so wrapping as direct ELF binaries —
					// without it the host's ld.so loads the rootfs's libc,
					// causing GLIBC_PRIVATE symbol mismatches.
					if shellInterp := readELFInterpreter(shellHost); shellInterp != "" {
						shellInterpHost, jerr2 := joinUnderRoot(app.RootfsPath, shellInterp)
						if jerr2 != nil {
							cancel()
							return nil, fmt.Errorf("invalid shebang ELF interpreter path: %w", jerr2)
						}
						if _, serr := os.Stat(shellInterpHost); serr == nil {
							libDirs := findRootfsLibDirs(app.RootfsPath)
							wrappedArgv := []string{shellInterpHost}
							if len(libDirs) > 0 {
								wrappedArgv = append(wrappedArgv, "--library-path", strings.Join(libDirs, ":"))
							}
							wrappedArgv = append(wrappedArgv, newArgv...)
							cmd.Path = shellInterpHost
							cmd.Args = wrappedArgv
						} else {
							cmd.Path = shellHost
							cmd.Args = newArgv
						}
					} else {
						cmd.Path = shellHost
						cmd.Args = newArgv
					}
				}
			}
		}
	}

	// Isolated env — no parent-process variables leak into the app.
	cmd.Env = buildEnv(app.EnvVars)

	// In non-chroot mode also inject LD_LIBRARY_PATH so that any secondary
	// dynamic loading (Python's ctypes, Node native addons, dlopen calls)
	// can find image libraries without going through the linker invocation.
	// Also rewrite PATH entries with the rootfs prefix so that subprocess
	// exec calls (#!/usr/bin/env, shell scripts calling bare commands like
	// pip/npm/node) resolve to host-accessible paths instead of rootfs-
	// relative paths that don't exist on the host filesystem.
	if !useChroot && app.RootfsPath != "" {
		cmd.Env = injectLibPaths(cmd.Env, app.RootfsPath)
		cmd.Env = prefixPathEnv(cmd.Env, app.RootfsPath)
		cmd.Env = appHomeUnderRootfs(cmd.Env, app.RootfsPath)
	}

	// Platform-specific: process group + optional chroot.
	chrootPath := ""
	if useChroot {
		chrootPath = app.RootfsPath
	}
	setSysProcAttr(cmd, chrootPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start process: %w", err)
	}

	p := &Process{cmd: cmd, cancel: cancel, logs: logs, exited: make(chan struct{}), done: make(chan struct{})}

	// Pipe stdout/stderr into the log buffer.
	go pipeLines(stdout, logs)
	go pipeLines(stderr, logs)

	// For the chroot case the exec.Cmd has no ctx wired in, so we watch the
	// context ourselves and force-kill the process group when it is cancelled
	// (cancel is only called from stop(), after the SIGTERM grace period).
	if useChroot {
		go func() {
			select {
			case <-ctx.Done():
				killProcessGroupForce(p.cmd.Process.Pid)
			case <-p.exited:
			}
		}()
	}

	return p, nil
}

// buildEnv assembles the environment slice for the child process.
// It always injects a standard PATH and HOME so bare commands resolve and
// user-home-dependent tools (pip, npm caches, etc.) have a valid $HOME.
func buildEnv(appEnv []store.EnvVar) []string {
	env := make([]string, 0, len(appEnv)+2)
	hasPath := false
	hasHome := false
	for _, ev := range appEnv {
		env = append(env, ev.Key+"="+ev.Value)
		if strings.EqualFold(ev.Key, "PATH") {
			hasPath = true
		}
		if strings.EqualFold(ev.Key, "HOME") {
			hasHome = true
		}
	}
	if !hasPath {
		// Standard POSIX search path; works both in chroot and on host.
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin")
	}
	if !hasHome {
		env = append(env, "HOME=/root")
	}
	// Windows needs SYSTEMROOT in the child environment for DNS resolution,
	// TLS root stores, and other OS facilities.
	if goruntime.GOOS == "windows" {
		if v := os.Getenv("SYSTEMROOT"); v != "" {
			env = append(env, "SYSTEMROOT="+v)
		}
	}
	return env
}

// findRootfsLibDirs returns all library directories that exist in the rootfs,
// covering the conventions used by Debian, Ubuntu, Alpine, and RHEL families.
func findRootfsLibDirs(rootfsPath string) []string {
	candidates := []string{
		"lib", "lib64",
		"usr/lib", "usr/lib64",
		"usr/local/lib",
		// Debian / Ubuntu multiarch
		"lib/x86_64-linux-gnu", "lib/aarch64-linux-gnu",
		"usr/lib/x86_64-linux-gnu", "usr/lib/aarch64-linux-gnu",
	}
	var found []string
	for _, d := range candidates {
		p := filepath.Join(rootfsPath, d)
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			found = append(found, p)
		}
	}
	return found
}

// injectLibPaths prepends the image's library directories to LD_LIBRARY_PATH
// so secondary dynamic loading (ctypes, dlopen, native addons) works without
// the explicit linker invocation.
func injectLibPaths(env []string, rootfsPath string) []string {
	dirs := findRootfsLibDirs(rootfsPath)
	if len(dirs) == 0 {
		return env
	}
	extra := strings.Join(dirs, ":")
	for i, e := range env {
		if strings.HasPrefix(e, "LD_LIBRARY_PATH=") {
			existing := strings.TrimPrefix(e, "LD_LIBRARY_PATH=")
			if existing != "" {
				env[i] = "LD_LIBRARY_PATH=" + extra + ":" + existing
			} else {
				env[i] = "LD_LIBRARY_PATH=" + extra
			}
			return env
		}
	}
	return append(env, "LD_LIBRARY_PATH="+extra)
}

// appHomeUnderRootfs repoints the default HOME (/root) at the app's own
// rootfs/tmp in prefix (non-chroot) mode. The default belongs to the host
// filesystem, which a rootless pocketpaas cannot write to — tools that cache
// in $HOME (pip, npm, Go) would fail. /tmp is recreated world-writable before
// every start (PruneRootfsKeepDNS) and is the app's own scratch space. A
// user-specified HOME is left untouched.
func appHomeUnderRootfs(env []string, rootfsPath string) []string {
	for i, e := range env {
		if e == "HOME=/root" {
			env[i] = "HOME=" + filepath.Join(rootfsPath, "tmp")
			break
		}
	}
	return env
}

// prefixPathEnv rewrites every entry in PATH by prepending rootfsPath, so that
// bare-name exec lookups (execvp, #!/usr/bin/env, shell "command not found")
// resolve to host-accessible absolute paths instead of rootfs-relative ones.
// For example, "/usr/bin" becomes "/home/.../rootfs/usr/bin".
func prefixPathEnv(env []string, rootfsPath string) []string {
	for i, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			raw := strings.TrimPrefix(e, "PATH=")
			parts := strings.Split(raw, ":")
			for j, p := range parts {
				if p != "" && filepath.IsAbs(p) {
					parts[j] = filepath.Join(rootfsPath, p)
				}
			}
			env[i] = "PATH=" + strings.Join(parts, ":")
			return env
		}
	}
	return env
}

// readELFInterpreter returns the dynamic linker path embedded in an ELF binary
// (the PT_INTERP segment), e.g. "/lib/ld-musl-x86_64.so.1".
// Returns "" for static binaries, non-ELF files, or unreadable files.
func readELFInterpreter(binaryPath string) string {
	f, err := elf.Open(binaryPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			buf := make([]byte, p.Filesz)
			if _, err := p.ReadAt(buf, 0); err != nil {
				return ""
			}
			// PT_INTERP is a null-terminated string.
			return strings.TrimRight(string(buf), "\x00")
		}
	}
	return "" // static binary — no interpreter needed
}

// readShebang parses the #! line of a script and returns the interpreter path
// and optional single argument (e.g. "/usr/bin/env", "python3" for #!/usr/bin/env python3).
// Returns ("", "") for non-scripts or unreadable files.
func readShebang(path string) (interp, arg string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	buf := make([]byte, 256)
	n, _ := f.Read(buf)
	line := string(buf[:n])
	if !strings.HasPrefix(line, "#!") {
		return "", ""
	}
	line = line[2:]
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	parts := strings.SplitN(line, " ", 2)
	interp = strings.TrimSpace(parts[0])
	if len(parts) == 2 {
		arg = strings.TrimSpace(parts[1])
	}
	return interp, arg
}

// isContainerAbs reports whether p is absolute in container-path semantics
// (starts with "/"). Unlike filepath.IsAbs this works on Windows dev builds,
// where container-style "/usr/bin/node" is not an absolute host path.
func isContainerAbs(p string) bool {
	return strings.HasPrefix(p, "/")
}

// lookPathInRootfs resolves a command name within the extracted rootfs.
//   - Absolute paths (e.g. "/usr/bin/python3") are returned unchanged; the
//     chroot maps them automatically.
//   - Bare names (e.g. "python", "node", "go") are searched in standard bin
//     dirs inside the rootfs; the chroot-relative absolute path is returned
//     (e.g. "/usr/bin/python3") so the kernel can exec it after chroot().
//   - Returns the original name if nothing is found; callers must treat that
//     as "not found in the image" (never fall back to the host PATH).
func lookPathInRootfs(name, rootfsPath string) string {
	if isContainerAbs(name) {
		return name
	}
	for _, dir := range []string{
		"/usr/local/bin", "/usr/bin", "/bin",
		"/usr/local/sbin", "/usr/sbin", "/sbin",
	} {
		hostPath := filepath.Join(rootfsPath, dir, name)
		if info, err := os.Stat(hostPath); err == nil && !info.IsDir() {
			return dir + "/" + name
		}
	}
	return name
}

// ensureChrootDNS copies /etc/resolv.conf and /etc/hosts from the host into
// the rootfs so DNS and hostname resolution work inside the chroot. Docker
// normally injects these at runtime; we do the same before exec().
// Writes never traverse planted rootfs symlinks (writeRootfsFileNoFollow).
func ensureChrootDNS(rootfsPath string) {
	for _, f := range []string{"/etc/resolv.conf", "/etc/hosts"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		writeRootfsFileNoFollow(rootfsPath, f, data, 0644)
	}
}

// PruneRootfsKeepDNS performs a safe prune of an extracted rootfs.
//
// It removes only things that are guaranteed never needed at runtime:
//   - Package-manager caches (/var/cache, /var/lib/apt, /var/lib/dpkg/info)
//   - Contents of the temp dirs (/tmp, /var/tmp — recreated empty with 1777)
//   - Documentation (/usr/share/doc, /usr/share/man, /usr/share/info)
//
// Everything else — shared libraries, language runtimes, Python/Node packages,
// binaries, and critically /etc (resolv.conf, nsswitch.conf, hosts) — is kept
// intact so the chrooted process continues to work at runtime.
//
// This runs BEFORE the app starts (never on a running app), so no runtime
// temp files are deleted out from under it. All removals and writes are
// containment-checked so a malicious rootfs with escaping symlinks (e.g.
// etc → /etc on the host) cannot redirect them outside the rootfs.
//
// Typical savings: 50–200 MB for a python:slim image, 20–80 MB for alpine.
func PruneRootfsKeepDNS(rootfsPath string) error {
	// Directories safe to delete: caches, docs, build-time junk.
	safeToRemove := []string{
		"var/cache",
		"var/lib/apt",
		"var/lib/dpkg/info",
		"var/tmp",
		"tmp",
		"usr/share/doc",
		"usr/share/man",
		"usr/share/info",
		"usr/share/locale",
		"root/.cache",
		// Note: "home" is intentionally NOT pruned — some images keep application
		// data or user configs there that are needed across restarts.
	}
	for _, rel := range safeToRemove {
		target, err := joinUnderRoot(rootfsPath, rel)
		if err != nil {
			continue
		}
		if !containedWithin(rootfsPath, target) {
			log.Printf("prune: refusing to remove %s — resolves outside the rootfs", target)
			continue
		}
		// RemoveAll removes a final-component symlink itself rather than
		// following it, so with the verified parent chain this is host-safe.
		_ = os.RemoveAll(target)
	}
	// Recreate the temp dirs removed above — they are RUNTIME directories,
	// and apps fail with ENOENT if /tmp does not exist.
	for _, rel := range []string{"tmp", "var/tmp"} {
		target, err := joinUnderRoot(rootfsPath, rel)
		if err != nil || !containedWithin(rootfsPath, filepath.Dir(target)) {
			continue
		}
		if err := os.MkdirAll(target, 0755); err == nil {
			_ = os.Chmod(target, 01777) // sticky bit, like a real /tmp
		}
	}
	// Refresh nameserver config from host so chrooted processes always see
	// up-to-date DNS (Docker / Northflank may rotate the nameserver IP).
	for _, f := range []string{"resolv.conf", "hosts"} {
		if data, err := os.ReadFile(filepath.Join("/etc", f)); err == nil {
			writeRootfsFileNoFollow(rootfsPath, "/etc/"+f, data, 0644)
		}
	}
	return nil
}

// maxLogLineBytes caps a single captured log line. Longer lines are truncated
// (with a marker) instead of killing the whole log stream, which is what
// bufio.Scanner did silently once a line exceeded its buffer.
const maxLogLineBytes = 64 * 1024

func pipeLines(r io.Reader, buf *LogBuffer) {
	br := bufio.NewReaderSize(r, 32*1024)
	var sb strings.Builder
	draining := false // discarding the remainder of an over-long line
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && !draining {
			if sb.Len()+len(chunk) > maxLogLineBytes {
				if keep := maxLogLineBytes - sb.Len(); keep > 0 {
					sb.Write(chunk[:keep])
				}
				sb.WriteString(" … [truncated]")
				draining = true
			} else {
				sb.Write(chunk)
			}
		}
		if err == bufio.ErrBufferFull {
			continue // line continues past the buffer — keep reading it
		}
		if sb.Len() > 0 {
			buf.Write(strings.TrimRight(sb.String(), "\r\n"))
			sb.Reset()
			draining = false
		}
		if err != nil { // io.EOF or a real read error — stream is over
			return
		}
	}
}

// markStopping records that this stop is intentional so the wait goroutine
// does not mark the app as crashed.
func (p *Process) markStopping() {
	atomic.StoreInt32(&p.stopping, 1)
}

// isStopping returns true if the stop was intentional.
func (p *Process) isStopping() bool {
	return atomic.LoadInt32(&p.stopping) == 1
}

const (
	// stopGracePeriod is how long an app has to exit after SIGTERM...
	stopGracePeriod = 10 * time.Second
	// ...before the process group is SIGKILLed, after which we wait this
	// long for teardown before giving up (the watcher still reaps it).
	stopKillPeriod = 5 * time.Second
)

// stop stops the process with escalation: SIGTERM to the whole process group,
// a grace period, then SIGKILL so a process that ignores SIGTERM is actually
// torn down. It returns once the process has exited (or the kill wait timed
// out, in which case the manager still finishes bookkeeping via the watcher).
func (p *Process) stop() {
	p.markStopping()
	if p.cmd.Process != nil {
		terminateProcessGroup(p.cmd.Process.Pid)
	}
	// Grace period: let the app shut down cleanly.
	select {
	case <-p.exited:
		p.cancel()
		return
	case <-time.After(stopGracePeriod):
	}
	// Escalate: SIGKILL the whole group, and cancel the context (non-chroot
	// CommandContext kills the leader; the chroot ctx-watcher force-kills).
	if p.cmd.Process != nil {
		killProcessGroupForce(p.cmd.Process.Pid)
	}
	p.cancel()
	select {
	case <-p.exited:
	case <-time.After(stopKillPeriod):
	}
}

// wait blocks until the process exits and returns the exit error (nil = clean exit).
func (p *Process) wait() error {
	err := p.cmd.Wait()
	close(p.exited)
	return err
}

// pid returns the OS process ID, or 0 if not started.
func (p *Process) pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}
