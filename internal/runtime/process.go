package runtime

import (
	"bufio"
	"context"
	"debug/elf"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"apphive/internal/store"
)

// Process wraps a running os/exec subprocess.
type Process struct {
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	logs     *LogBuffer
	done     chan struct{}
	stopping int32 // atomic; 1 = intentional stop (don't mark as crashed)
}

// startProcess launches a subprocess for the given app.
// The caller must hold the manager lock while calling this.
func startProcess(app *store.App, logs *LogBuffer) (*Process, error) {
	if len(app.Entrypoint) == 0 && len(app.Command) == 0 {
		return nil, fmt.Errorf("no entrypoint or command configured — pull the image first or set an override")
	}

	rawArgv := append(app.Entrypoint, app.Command...)
	useChroot := app.RootfsPath != "" && chrootEnabled

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
		// Step 1: prefix every absolute argv token with the rootfs path so
		// the kernel can find and exec the binary on the host filesystem.
		argv := make([]string, len(rawArgv))
		for i, a := range rawArgv {
			if app.RootfsPath != "" && filepath.IsAbs(a) {
				argv[i] = filepath.Join(app.RootfsPath, a)
			} else {
				argv[i] = a
			}
		}
		// If argv[0] is a bare name (e.g. "node", "python3") resolve it
		// inside the rootfs — otherwise exec.CommandContext would search
		// the HOST PATH and exec the wrong (or no) binary.
		if app.RootfsPath != "" && !filepath.IsAbs(argv[0]) {
			if resolved := lookPathInRootfs(argv[0], app.RootfsPath); filepath.IsAbs(resolved) {
				argv[0] = filepath.Join(app.RootfsPath, resolved)
			}
		}
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)

		if app.RootfsPath != "" {
			if app.WorkDir != "" {
				cmd.Dir = filepath.Join(app.RootfsPath, app.WorkDir)
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
				interpHost := filepath.Join(app.RootfsPath, interp)
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
				shellHost := filepath.Join(app.RootfsPath, shell)
				if _, err := os.Stat(shellHost); err == nil {
					newArgv := []string{shellHost}
					if arg != "" {
						newArgv = append(newArgv, arg)
					}
					newArgv = append(newArgv, argv...)
					cmd.Path = shellHost
					cmd.Args = newArgv
				}
			}
		}
	}

	// Isolated env — no parent-process variables leak into the app.
	cmd.Env = buildEnv(app.EnvVars)

	// In non-chroot mode also inject LD_LIBRARY_PATH so that any secondary
	// dynamic loading (Python's ctypes, Node native addons, dlopen calls)
	// can find image libraries without going through the linker invocation.
	if !useChroot && app.RootfsPath != "" {
		cmd.Env = injectLibPaths(cmd.Env, app.RootfsPath)
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

	p := &Process{cmd: cmd, cancel: cancel, logs: logs, done: make(chan struct{})}

	// Pipe stdout/stderr into the log buffer.
	go pipeLines(stdout, logs)
	go pipeLines(stderr, logs)

	// For the chroot case the exec.Cmd has no ctx wired in, so we watch the
	// context ourselves and kill the process group when it is cancelled.
	if useChroot {
		go func() {
			select {
			case <-ctx.Done():
				killProcessGroup(p.cmd.Process.Pid)
			case <-p.done:
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

// lookPathInRootfs resolves a command name within the extracted rootfs.
//   - Absolute paths (e.g. "/usr/bin/python3") are returned unchanged; the
//     chroot maps them automatically.
//   - Bare names (e.g. "python", "node", "go") are searched in standard bin
//     dirs inside the rootfs; the chroot-relative absolute path is returned
//     (e.g. "/usr/bin/python3") so the kernel can exec it after chroot().
//   - Returns the original name if nothing is found; exec() surfaces the error.
func lookPathInRootfs(name, rootfsPath string) string {
	if filepath.IsAbs(name) {
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
func ensureChrootDNS(rootfsPath string) {
	for _, f := range []string{"/etc/resolv.conf", "/etc/hosts"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		dst := filepath.Join(rootfsPath, f)
		_ = os.MkdirAll(filepath.Dir(dst), 0755)
		_ = os.WriteFile(dst, data, 0644)
	}
}

// PruneRootfsKeepDNS performs a safe prune of an extracted rootfs.
//
// It removes only things that are guaranteed never needed at runtime:
//   - Package-manager caches (/var/cache, /var/lib/apt, /var/lib/dpkg/info)
//   - Build-time temp files (/tmp, /var/tmp)
//   - Documentation (/usr/share/doc, /usr/share/man, /usr/share/info)
//
// Everything else — shared libraries, language runtimes, Python/Node packages,
// binaries, and critically /etc (resolv.conf, nsswitch.conf, hosts) — is kept
// intact so the chrooted process continues to work at runtime.
//
// Typical savings: 50–200 MB for a python:slim image, 20–80 MB for alpine.
// After pruning it also refreshes the host DNS config in /etc so long-running
// apps always have up-to-date nameserver addresses.
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
		"home",
	}
	for _, rel := range safeToRemove {
		_ = os.RemoveAll(filepath.Join(rootfsPath, rel))
	}
	// Refresh nameserver config from host so chrooted processes always see
	// up-to-date DNS (Docker / Northflank may rotate the nameserver IP).
	for _, f := range []string{"resolv.conf", "hosts"} {
		if data, err := os.ReadFile(filepath.Join("/etc", f)); err == nil {
			dst := filepath.Join(rootfsPath, "etc", f)
			_ = os.MkdirAll(filepath.Dir(dst), 0755)
			_ = os.WriteFile(dst, data, 0644)
		}
	}
	return nil
}

const scannerBufSize = 512 * 1024 // 512 KB — handles most log lines

func pipeLines(r io.Reader, buf *LogBuffer) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, scannerBufSize), scannerBufSize)
	for scanner.Scan() {
		buf.Write(scanner.Text())
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

// stop signals the process group to exit gracefully, then cancels the context.
func (p *Process) stop() {
	p.markStopping()
	if p.cmd.Process != nil {
		killProcessGroup(p.cmd.Process.Pid)
		p.cmd.Process.Signal(os.Interrupt)
	}
	p.cancel()
}

// wait blocks until the process exits and returns the exit error (nil = clean exit).
func (p *Process) wait() error {
	err := p.cmd.Wait()
	close(p.done)
	return err
}

// pid returns the OS process ID, or 0 if not started.
func (p *Process) pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}
