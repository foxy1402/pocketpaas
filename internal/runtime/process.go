package runtime

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

	// Resolve argv: if a rootfs is available, prefix absolute paths with it
	// so the container binary is found on the host filesystem.
	rawArgv := append(app.Entrypoint, app.Command...)
	argv := make([]string, len(rawArgv))
	for i, a := range rawArgv {
		if app.RootfsPath != "" && filepath.IsAbs(a) {
			argv[i] = filepath.Join(app.RootfsPath, a)
		} else {
			argv[i] = a
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)

	// Set working directory: rootfs + container WORKDIR, or just rootfs, or WORKDIR alone.
	if app.RootfsPath != "" {
		if app.WorkDir != "" {
			cmd.Dir = filepath.Join(app.RootfsPath, app.WorkDir)
		} else {
			cmd.Dir = app.RootfsPath
		}
	} else if app.WorkDir != "" {
		cmd.Dir = app.WorkDir
	}

	// Isolated env — no parent env inheritance.
	cmd.Env = make([]string, 0, len(app.EnvVars))
	for _, ev := range app.EnvVars {
		cmd.Env = append(cmd.Env, ev.Key+"="+ev.Value)
	}

	// Use a process group so we can kill all children on stop (Linux/macOS).
	setSysProcAttr(cmd)

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

	// Pipe stdout and stderr into the log buffer.
	go pipeLines(stdout, logs)
	go pipeLines(stderr, logs)

	return p, nil
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
