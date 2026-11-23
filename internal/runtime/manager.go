package runtime

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"apphive/internal/store"
)

// HealthState represents the last known health check result.
type HealthState int

const (
	HealthUnknown HealthState = iota
	HealthOK
	HealthFailing
)

func (h HealthState) String() string {
	switch h {
	case HealthOK:
		return "ok"
	case HealthFailing:
		return "failing"
	default:
		return "unknown"
	}
}

// DeployState holds the log and completion signal for an active sequential deploy.
type DeployState struct {
	Log  *LogBuffer
	Done chan struct{}
}

// Manager controls the lifecycle of all app subprocesses.
type Manager struct {
	mu      sync.Mutex
	procs   map[string]*Process           // appID -> active process
	logs    map[string]*LogBuffer         // appID -> log buffer (persists across restarts)
	health  map[string]HealthState        // appID -> last health check result
	pulls   map[string]chan string        // appID -> pull-progress channel
	pingers map[string]context.CancelFunc // appID -> cancel func for health pinger
	opLocks map[string]*sync.Mutex        // appID -> serializes start/stop/pull/delete/deploy
	deploy  *DeployState                  // non-nil when deploy is active or recently done
	store   *store.AppStore
	dataDir string
}

func NewManager(s *store.AppStore, dataDir string) *Manager {
	return &Manager{
		procs:   make(map[string]*Process),
		logs:    make(map[string]*LogBuffer),
		health:  make(map[string]HealthState),
		pulls:   make(map[string]chan string),
		pingers: make(map[string]context.CancelFunc),
		opLocks: make(map[string]*sync.Mutex),
		store:   s,
		dataDir: dataDir,
	}
}

// errBusy is returned when a per-app operation lock is already held.
var errBusy = fmt.Errorf("another operation is in progress for this app")

// getOpLock returns (creating if necessary) the per-app operation lock.
func (m *Manager) getOpLock(appID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.opLocks[appID]
	if l == nil {
		l = &sync.Mutex{}
		m.opLocks[appID] = l
	}
	return l
}

// TryLockApp acquires the per-app operation lock without blocking.
// Returns (unlock, true) or (nil, false) if another operation holds it.
func (m *Manager) TryLockApp(appID string) (func(), bool) {
	l := m.getOpLock(appID)
	if !l.TryLock() {
		return nil, false
	}
	return l.Unlock, true
}

// LockApp acquires the per-app operation lock, blocking until available.
// Used by long background operations (pulls/deploys) that must wait their
// turn rather than fail.
func (m *Manager) LockApp(appID string) func() {
	l := m.getOpLock(appID)
	l.Lock()
	return l.Unlock
}

// StartDeploy marks the start of a sequential deploy and returns its state.
// Returns nil if a deploy is already in progress.
func (m *Manager) StartDeploy() *DeployState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deploy != nil {
		select {
		case <-m.deploy.Done:
			// Previous deploy finished — allow a new one.
		default:
			return nil // still running
		}
	}
	m.deploy = &DeployState{
		Log:  newLogBuffer(),
		Done: make(chan struct{}),
	}
	return m.deploy
}

// FinishDeploy signals the sequential deploy as complete.
func (m *Manager) FinishDeploy() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deploy == nil {
		return
	}
	// The check and the close both happen under m.mu, so double-close is
	// impossible by construction.
	select {
	case <-m.deploy.Done:
	default:
		close(m.deploy.Done)
	}
}

// CurrentDeploy returns the most recent deploy state (may be finished), or nil.
func (m *Manager) CurrentDeploy() *DeployState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deploy
}

// LogBuffer returns (creating if necessary) the log buffer for an app.
func (m *Manager) LogBuffer(appID string) *LogBuffer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.logs[appID] == nil {
		m.logs[appID] = newLogBuffer()
	}
	return m.logs[appID]
}

// Start starts the app subprocess.
// It holds the app's operation lock for the whole spawn so concurrent
// stop/pull/delete operations on the same app cannot interleave, but never
// holds the global manager lock across disk I/O or SQL queries.
func (m *Manager) Start(appID string) error {
	unlock, ok := m.TryLockApp(appID)
	if !ok {
		return errBusy
	}
	defer unlock()

	m.mu.Lock()
	if _, running := m.procs[appID]; running {
		m.mu.Unlock()
		return fmt.Errorf("app is already running")
	}
	if _, pulling := m.pulls[appID]; pulling {
		m.mu.Unlock()
		return fmt.Errorf("image pull in progress")
	}
	m.mu.Unlock()

	app, err := m.store.Get(appID)
	if err != nil || app == nil {
		return fmt.Errorf("app not found")
	}
	if app.Status == store.StatusPulling {
		return fmt.Errorf("image pull in progress")
	}
	if app.RootfsPath == "" {
		return fmt.Errorf("image rootfs missing; pull the image before starting")
	}

	buf := m.LogBuffer(appID)

	// Prune safe cache/doc/temp directories BEFORE starting (never while the
	// app runs): the app always sees a freshly recreated empty /tmp, and no
	// runtime file can disappear out from under it mid-flight.
	if app.PruneAfterStart && app.RootfsPath != "" {
		if err := PruneRootfsKeepDNS(app.RootfsPath); err != nil {
			log.Printf("prune rootfs %s: %v", appID, err)
		} else {
			log.Printf("pruned caches for app %s (%s, kept runtime files and DNS)", appID, app.RootfsPath)
		}
	}

	proc, err := startProcess(app, buf)
	if err != nil && isEPERM(err) && app.RootfsPath != "" {
		// The boot probe only proves that the probe process can use the
		// isolation syscalls. The spawned child uses a different syscall shape
		// (Go clones with CLONE_VFORK|CLONE_VM unless a new user namespace is
		// requested), so exec-time policy (seccomp/LSM/runtime) can still deny
		// it. Self-heal: demote down the ladder — chroot → virtual root →
		// proot → rootfs-prefix — retrying the start at each tier.
		log.Printf("runtime: spawn of app %s denied in %s mode: %v", appID, IsolationMode(), err)
		log.Printf("runtime: environment: %s", SpawnDiagnostics(app.RootfsPath))
		for Demote(err) {
			log.Printf("runtime: retrying app %s in %s mode", appID, IsolationMode())
			proc, err = startProcess(app, buf)
			if err == nil || !isEPERM(err) {
				break
			}
			log.Printf("runtime: %s spawn of app %s denied too: %v", IsolationMode(), appID, err)
		}
	}
	if err != nil {
		_ = m.store.UpdateStatus(appID, store.StatusError)
		return err
	}

	m.mu.Lock()
	m.procs[appID] = proc
	m.mu.Unlock()
	_ = m.store.UpdateStatus(appID, store.StatusRunning)
	_ = m.store.SetLastStarted(appID)
	log.Printf("app %s started (isolation: %s, pid %d)", appID, IsolationMode(), proc.pid())

	// Watch the process in a goroutine; update status when it exits.
	go func() {
		exitErr := proc.wait()
		// Remove from the registry BEFORE closing done: Stop() returning must
		// imply the slot is free so an immediate Start() can't see the stale
		// entry (previously done closed first → spurious "already running").
		m.mu.Lock()
		delete(m.procs, appID)
		m.mu.Unlock()
		close(proc.done)

		if proc.isStopping() {
			// Intentional stop — don't overwrite the status set by Stop().
			return
		}
		if exitErr != nil {
			log.Printf("app %s exited with error: %v", appID, exitErr)
			_ = m.store.UpdateStatus(appID, store.StatusCrashed)
		} else {
			_ = m.store.UpdateStatus(appID, store.StatusStopped)
		}
	}()

	return nil
}

// Stop stops the app subprocess (SIGTERM → grace → SIGKILL) and only marks it
// stopped after the process has actually exited and been removed from the
// process registry.
func (m *Manager) Stop(appID string) error {
	unlock, ok := m.TryLockApp(appID)
	if !ok {
		return errBusy
	}
	defer unlock()
	return m.StopLocked(appID)
}

// StopLocked is Stop for callers that already hold the app's operation lock
// (e.g. deleteApp, sequential deploy).
func (m *Manager) StopLocked(appID string) error {
	m.mu.Lock()
	proc, ok := m.procs[appID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("app is not running")
	}

	proc.stop() // blocks through the grace period and SIGKILL escalation

	// The watcher closes done only after removing the process from the
	// registry, so once Stop returns the app can be started again cleanly.
	<-proc.done

	_ = m.store.UpdateStatus(appID, store.StatusStopped)
	return nil
}

// Restart stops then starts the app. Start() re-reads the app from the store
// so the new process always picks up the latest config.
func (m *Manager) Restart(appID string) error {
	if m.IsRunning(appID) {
		if err := m.Stop(appID); err != nil {
			return err
		}
	}
	return m.Start(appID)
}

// IsRunning returns true if the app subprocess is active.
func (m *Manager) IsRunning(appID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.procs[appID]
	return ok
}

// PID returns the PID of the running app, or 0.
func (m *Manager) PID(appID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.procs[appID]; ok {
		return p.pid()
	}
	return 0
}

// TryStartPull atomically checks whether a pull is already in progress for
// the app and, if not, registers its progress channel. It also acquires the
// per-app operation lock for the duration of the pull (returned as the
// release func), so start/stop/delete/deploy operations on the same app
// cannot interleave with the pull+extract. Callers defer ClosePullChannel
// and the release func.
func (m *Manager) TryStartPull(appID string) (ch chan string, release func(), ok bool) {
	unlock := m.LockApp(appID)
	m.mu.Lock()
	if _, busy := m.pulls[appID]; busy {
		m.mu.Unlock()
		unlock()
		return nil, nil, false
	}
	ch = make(chan string, 64)
	m.pulls[appID] = ch
	m.mu.Unlock()
	return ch, unlock, true
}

// PullChannel returns the active pull progress channel, or nil.
func (m *Manager) PullChannel(appID string) chan string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pulls[appID]
}

// ClosePullChannel closes and removes the pull progress channel.
func (m *Manager) ClosePullChannel(appID string) {
	m.mu.Lock()
	ch, ok := m.pulls[appID]
	if ok {
		delete(m.pulls, appID)
	}
	m.mu.Unlock()
	if ok {
		close(ch)
	}
}

// RemoveApp cleans up all in-memory state for a deleted app.
// The caller must hold the app's operation lock (guarantees no pull or
// process operation is in flight for this app).
func (m *Manager) RemoveApp(appID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.logs, appID)
	delete(m.health, appID)
	delete(m.opLocks, appID)
	if cancel, ok := m.pingers[appID]; ok {
		cancel()
		delete(m.pingers, appID)
	}
}

// StartHealthPinger begins a background goroutine that pings the app's health
// endpoint every 15 s. Any existing pinger for the same app is cancelled first.
func (m *Manager) StartHealthPinger(app *store.App) {
	m.mu.Lock()
	if cancel, ok := m.pingers[app.ID]; ok {
		cancel() // stop old pinger
		delete(m.pingers, app.ID)
	}
	delete(m.health, app.ID)
	if app.HealthEndpoint == "" || app.ExposedPort == 0 || !validHealthPath(app.HealthEndpoint) {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.pingers[app.ID] = cancel
	m.mu.Unlock()

	url := fmt.Sprintf("http://localhost:%d%s", app.ExposedPort, app.HealthEndpoint)

	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		client := &http.Client{Timeout: 3 * time.Second}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state := HealthFailing
				if m.IsRunning(app.ID) {
					if resp, err := client.Get(url); err == nil {
						if resp.StatusCode < 400 {
							state = HealthOK
						}
						resp.Body.Close()
					}
				}
				m.mu.Lock()
				m.health[app.ID] = state
				m.mu.Unlock()
			}
		}
	}()
}

// HealthStatus returns the last known health check state (unknown/ok/failing).
func (m *Manager) HealthStatus(appID string) HealthState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.health[appID]
}

// StopAll terminates every running app process. Called during graceful
// shutdown so no app subprocesses are orphaned.
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

// validHealthPath reports whether s is a safe local path (e.g. "/health")
// rather than an absolute URL that would leak requests to an external server.
func validHealthPath(s string) bool {
	return strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "//") && !strings.Contains(s, "://")
}
