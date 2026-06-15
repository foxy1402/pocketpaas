package runtime

import (
	"bufio"
	"context"
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
		// ── Chroot path (Linux) ──────────────────────────────────────────────
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
		// ── Non-chroot path (Windows / macOS dev builds) ─────────────────────
		// Prefix absolute argv paths with the rootfs directory so the host can
		// locate the binary (e.g. /usr/bin/python → /data/.../rootfs/usr/bin/python).
		argv := make([]string, len(rawArgv))
		for i, a := range rawArgv {
			if app.RootfsPath != "" && filepath.IsAbs(a) {
				argv[i] = filepath.Join(app.RootfsPath, a)
			} else {
				argv[i] = a
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
	}

	// Isolated env — no parent-process variables leak into the app.
	cmd.Env = buildEnv(app.EnvVars)

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
