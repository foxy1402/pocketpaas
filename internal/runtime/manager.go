package runtime

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"apphive/internal/sysinfo"
	"apphive/internal/store"
)

// AppStat holds a per-process resource snapshot (CPU % and RSS in MB).
type AppStat struct {
	CPUPct float64
	RAMMB  float64
}

type procTick struct {
	ticks uint64
	at    time.Time
}

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
	pulls   map[string]chan string         // appID -> pull-progress channel
	pingers map[string]context.CancelFunc  // appID -> cancel func for health pinger
	deploy  *DeployState                   // non-nil when deploy is active or recently done
	store   *store.AppStore
	dataDir string

	statsMu   sync.RWMutex
	procStats map[string]AppStat  // appID -> latest resource snapshot
	prevTicks map[string]procTick // appID -> previous CPU tick sample
}

func NewManager(s *store.AppStore, dataDir string) *Manager {
	m := &Manager{
		procs:     make(map[string]*Process),
		logs:      make(map[string]*LogBuffer),
		health:    make(map[string]HealthState),
		pulls:     make(map[string]chan string),
		pingers:   make(map[string]context.CancelFunc),
		procStats: make(map[string]AppStat),
		prevTicks: make(map[string]procTick),
		store:     s,
		dataDir:   dataDir,
	}
	go m.statsPoll()
	return m
}

// statsPoll runs in the background and refreshes per-app stats every 5 s.
func (m *Manager) statsPoll() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.updateProcStats()
	}
}

// updateProcStats samples /proc/{pid}/stat for every running process.
func (m *Manager) updateProcStats() {
	m.mu.Lock()
	pids := make(map[string]int, len(m.procs))
	for id, proc := range m.procs {
		pids[id] = proc.pid()
	}
	m.mu.Unlock()

	now := time.Now()

	m.statsMu.RLock()
	prev := m.prevTicks
	m.statsMu.RUnlock()

	newStats := make(map[string]AppStat, len(pids))
	newTicks := make(map[string]procTick, len(pids))

	for appID, pid := range pids {
		if pid <= 0 {
			continue
		}
		ticks, rssKB, err := sysinfo.ReadProcStat(pid)
		if err != nil {
			continue
		}
		stat := AppStat{RAMMB: float64(rssKB) / 1024.0}
		if p, ok := prev[appID]; ok && p.ticks > 0 {
			elapsed := now.Sub(p.at).Seconds()
			if elapsed >= 0.5 {
				delta := int64(ticks) - int64(p.ticks)
				if delta > 0 {
					stat.CPUPct = float64(delta) / (elapsed * float64(sysinfo.ClkTck())) * 100
					if stat.CPUPct > 100 {
						stat.CPUPct = 100
					}
				}
			}
		}
		newStats[appID] = stat
		newTicks[appID] = procTick{ticks: ticks, at: now}
	}

	m.statsMu.Lock()
	m.procStats = newStats
	m.prevTicks = newTicks
	m.statsMu.Unlock()
}

// AppStat returns the latest CPU/RAM snapshot for appID, or zero if unavailable.
func (m *Manager) AppStat(appID string) AppStat {
	m.statsMu.RLock()
	defer m.statsMu.RUnlock()
	return m.procStats[appID]
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
	d := m.deploy
	m.mu.Unlock()
	if d != nil {
		select {
		case <-d.Done:
		default:
			close(d.Done)
		}
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
func (m *Manager) Start(appID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.procs[appID]; ok {
		return fmt.Errorf("app is already running")
	}

	app, err := m.store.Get(appID)
	if err != nil || app == nil {
		return fmt.Errorf("app not found")
	}
	if app.Status == store.StatusPulling {
		return fmt.Errorf("image pull in progress")
	}

	if m.logs[appID] == nil {
		m.logs[appID] = newLogBuffer()
	}

	proc, err := startProcess(app, m.logs[appID])
	if err != nil {
		_ = m.store.UpdateStatus(appID, store.StatusError)
		return err
	}

	m.procs[appID] = proc
	_ = m.store.UpdateStatus(appID, store.StatusRunning)
	_ = m.store.SetLastStarted(appID)

	// Watch the process in a goroutine; update status when it exits.
	go func() {
		exitErr := proc.wait()
		m.mu.Lock()
		delete(m.procs, appID)
		m.mu.Unlock()

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

	// Prune rootfs after a short delay if requested.
	// On Linux a running process keeps its loaded files open even after the
	// directory is deleted — the disk space is freed immediately.
	if app.PruneAfterStart && app.RootfsPath != "" {
		rootfs := app.RootfsPath
		go func() {
			time.Sleep(3 * time.Second)
			if !m.IsRunning(appID) {
				// Crashed immediately — keep rootfs for debugging.
				return
			}
			if err := PruneRootfsKeepDNS(rootfs); err != nil {
				log.Printf("prune rootfs %s: %v", appID, err)
				return
			}
			log.Printf("pruned rootfs for app %s (%s, kept /etc for DNS)", appID, rootfs)
			_ = m.store.UpdateRootfsPath(appID, "")
		}()
	}

	return nil
}

// Stop stops the app subprocess, waits up to 10 s, then force-kills via context.
func (m *Manager) Stop(appID string) error {
	m.mu.Lock()
	proc, ok := m.procs[appID]
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("app is not running")
	}

	proc.stop()

	select {
	case <-proc.done:
	case <-time.After(10 * time.Second):
	}

	_ = m.store.UpdateStatus(appID, store.StatusStopped)
	return nil
}

// Restart stops then starts the app.
func (m *Manager) Restart(appID string) error {
	m.mu.Lock()
	_, running := m.procs[appID]
	m.mu.Unlock()

	if running {
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

// OpenPullChannel creates a progress channel for a pull operation.
func (m *Manager) OpenPullChannel(appID string) chan string {
	ch := make(chan string, 64)
	m.mu.Lock()
	m.pulls[appID] = ch
	m.mu.Unlock()
	return ch
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
func (m *Manager) RemoveApp(appID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.logs, appID)
	delete(m.health, appID)
	if cancel, ok := m.pingers[appID]; ok {
		cancel()
		delete(m.pingers, appID)
	}
}

// StartHealthPinger begins a background goroutine that pings the app's health
// endpoint every 15 s. Any existing pinger for the same app is cancelled first.
func (m *Manager) StartHealthPinger(app *store.App) {
	if app.HealthEndpoint == "" || app.ExposedPort == 0 {
		return
	}
	url := fmt.Sprintf("http://localhost:%d%s", app.ExposedPort, app.HealthEndpoint)

	m.mu.Lock()
	if cancel, ok := m.pingers[app.ID]; ok {
		cancel() // stop old pinger
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.pingers[app.ID] = cancel
	m.mu.Unlock()

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
					resp, err := client.Get(url)
					if err == nil && resp.StatusCode < 400 {
						state = HealthOK
					}
					if resp != nil {
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
