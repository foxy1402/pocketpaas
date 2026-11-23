package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"apphive/internal/api"
	"apphive/internal/auth"
	"apphive/internal/registry"
	"apphive/internal/runtime"
	"apphive/internal/store"
	"apphive/internal/sysinfo"
	"apphive/internal/tunnel"
)

//go:embed all:web
var webFS embed.FS

// Build-time variables injected by Dockerfile ldflags (-X main.buildTime=... -X main.revision=...).
var (
	buildTime string
	revision  string
)

func main() {
	// Diagnostics child — spawned by `--diagnose` inside a chroot to prove
	// that clone+chroot+execve works in this environment. Exits immediately.
	if len(os.Args) > 1 && os.Args[1] == "--internal-diag-child" {
		os.Exit(0)
	}

	// Internal probe child — re-executed by runtime at startup to test whether
	// unprivileged user namespaces allow chroot ("virtual root") on rootless
	// hosts. Never used during normal server operation.
	if len(os.Args) > 1 && os.Args[1] == "--internal-userns-probe" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: --internal-userns-probe <dir>")
			os.Exit(2)
		}
		if err := runtime.RunUsernsProbe(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "virtual-root probe failed:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Hidden support flag: report the isolation capabilities of this
	// environment (capabilities, seccomp, no_new_privs, mount flags, and
	// probe results for each ladder tier). Useful when a PaaS runtime denies
	// app spawns.
	if len(os.Args) > 1 && os.Args[1] == "--diagnose" {
		runtime.Diagnose(os.Stdout)
		os.Exit(0)
	}

	// Hidden healthcheck flag: the runtime image installs no HTTP client, so
	// the Docker HEALTHCHECK asks the binary itself instead of wget/curl.
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Get("http://127.0.0.1:" + envOrDefault("PORT", "8080") + "/login")
		if err != nil {
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode >= http.StatusBadRequest {
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Pick the strongest isolation mode available (chroot → virtual root →
	// rootfs prefix) and log it. Must run after the probe-child branch above:
	// the probe re-executes this binary, and the child must not probe again.
	runtime.InitIsolation()

	password := os.Getenv("DASHBOARD_PASSWORD")
	if password == "" {
		log.Fatal("DASHBOARD_PASSWORD environment variable is required")
	}

	// Resolve the data directory. A rootless container typically inherits
	// DATA_DIR=/data from the image (or a volume mounted at /data owned by
	// root), so verify the directory is actually writable instead of only
	// ensuring it exists — and fall back to a writable location rather than
	// refusing to boot.
	dataDir := envOrDefault("DATA_DIR", autoDataDir())
	if err := ensureWritableDir(dataDir); err != nil {
		fallback := autoDataDir()
		if fallback == dataDir || ensureWritableDir(fallback) != nil {
			log.Fatalf("data dir %s is not writable (%v) and no writable fallback was found — set DATA_DIR to a writable path", dataDir, err)
		}
		log.Printf("warn: data dir %s is not writable (%v) — using %s instead (set DATA_DIR to override)", dataDir, err, fallback)
		dataDir = fallback
	}

	port := envOrDefault("PORT", "8080")

	ngrokToken := os.Getenv("NGROK_AUTHTOKEN")
	ngrokDomain := os.Getenv("NGROK_DOMAIN")

	regCreds := &registry.Credentials{
		Username: os.Getenv("REGISTRY_USER"),
		Password: os.Getenv("REGISTRY_PASSWORD"),
	}

	// Ensure data directory exists.
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}

	// On-disk layer cache. Defaults to the OS temp dir: ephemeral storage is
	// plentiful on most free tiers and the cache is wiped on restart, so it
	// never eats persistent-volume quota. Set CACHE_DIR for a persistent
	// location, CACHE_MAX_GB to change the eviction cap.
	layerCache := &registry.LayerCache{
		Dir:      envOrDefault("CACHE_DIR", filepath.Join(os.TempDir(), "pocketpaas-cache")),
		MaxBytes: int64(envIntOrDefault("CACHE_MAX_GB", 10)) << 30,
	}
	layerCache.Prune() // startup cleanup: stale temp files + over-cap blobs

	// Open SQLite database.
	dbPath := filepath.Join(dataDir, "apphive.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

	appStore := store.NewAppStore(db)

	// Reset any apps that were recorded as "running" at last shutdown.
	if err := appStore.ResetRunningToStopped(); err != nil {
		log.Printf("warn: reset running status: %v", err)
	}

	// Tunnel manager — nil (no-op) when NGROK_AUTHTOKEN is not set.
	var tunMgr *tunnel.Manager
	if ngrokToken != "" {
		tunMgr = &tunnel.Manager{}
	}

	// Bootstrap runtime manager.
	mgr := runtime.NewManager(appStore)

	// Auth manager (+ background expiry sweep for abandoned sessions).
	authMgr := auth.NewManager(password)

	// bgCtx is cancelled during graceful shutdown: the ngrok tunnel,
	// background pulls/deploys, and the session sweeper all stop on it.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	authMgr.StartSweeper(bgCtx)

	// System resource sampler (CPU %, RAM MB) — updated every 5 s.
	sampler := &sysinfo.Sampler{}
	sampler.Update() // prime with first reading before the server starts
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			sampler.Update()
		}
	}()

	// Build HTTP server.
	mux := http.NewServeMux()
	srv := api.NewServer(appStore, mgr, authMgr, webFS, dataDir, regCreds, sampler, tunMgr, layerCache, bgCtx)
	srv.RegisterRoutes(mux, webFS)
	handler := api.SecurityHeaders(mux)

	// Start ngrok tunnel (no-op when NGROK_AUTHTOKEN is not set).
	if tunMgr != nil {
		tunMgr.Start(bgCtx, handler, ngrokToken, ngrokDomain)
	}

	// Auto-start apps that have the flag set (only meaningful with persistent storage).
	autoStartApps(appStore, mgr)

	// Start health pingers for apps that have a health endpoint.
	startHealthPingers(appStore, mgr)

	addr := fmt.Sprintf(":%s", port)
	rev := revision
	if rev == "" {
		rev = "dev"
	}

	// Graceful shutdown on SIGTERM/SIGINT:
	//   1. srvHTTP.Shutdown stops NEW requests first, so no handler can still
	//      start/stop/delete apps while we tear the world down.
	//   2. bgCancel aborts background pulls/deploys (they extract to a staging
	//      dir, so they leave no half-written rootfs) and closes the tunnel.
	//   3. StopAll stops app subprocesses last.
	srvHTTP := &http.Server{
		Addr:    addr,
		Handler: handler,
		// Timeouts guard against Slowloris-style resource exhaustion.
		// WriteTimeout stays 0: SSE log/pull/deploy streams are unbounded.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Println("shutting down — draining HTTP, stopping apps…")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srvHTTP.Shutdown(ctx); err != nil {
			log.Printf("warn: http shutdown: %v", err)
		}
		bgCancel()
		mgr.StopAll()
	}()

	log.Printf("pocketpaas %s (built %s) listening on %s (data: %s)", rev, buildTime, addr, dataDir)
	if err := srvHTTP.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		// Don't orphan the auto-started apps on a listen failure (e.g. port
		// already in use): stop everything before exiting.
		mgr.StopAll()
		log.Fatalf("listen: %v", err)
	}
}

func autoStartApps(s *store.AppStore, mgr *runtime.Manager) {
	apps, err := s.List()
	if err != nil {
		log.Printf("warn: auto-start list: %v", err)
		return
	}
	for _, app := range apps {
		if app.AutoStart && app.RootfsPath != "" {
			log.Printf("auto-starting app %s (%s)", app.Name, app.ID)
			if err := mgr.Start(app.ID); err != nil {
				log.Printf("warn: auto-start %s: %v", app.Name, err)
			}
		}
	}
}

func startHealthPingers(s *store.AppStore, mgr *runtime.Manager) {
	apps, err := s.List()
	if err != nil {
		return
	}
	for _, app := range apps {
		if app.HealthEndpoint != "" && app.ExposedPort > 0 {
			mgr.StartHealthPinger(app)
		}
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		log.Printf("warn: invalid %s=%q — using default %d", key, v, def)
	}
	return def
}

// autoDataDir picks a writable data directory without requiring configuration.
// It prefers /data (Docker volume convention), and falls back to
// ~/.pocketpaas/data (SSH / non-root container scenario).
// ensureWritableDir creates path if needed and proves it is writable by
// writing a probe file. MkdirAll alone is not enough: an existing directory
// owned by another user (e.g. /data from the image or a root-owned volume)
// makes MkdirAll succeed while every later write fails.
func ensureWritableDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(path, ".write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func autoDataDir() string {
	// /data is the image's Docker volume mount; use it when writable.
	if ensureWritableDir("/data") == nil {
		return "/data"
	}
	// Rootless runs: fall back to the user's home directory when available.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p := filepath.Join(home, ".pocketpaas", "data"); ensureWritableDir(p) == nil {
			return p
		}
	}
	// Last resort: the OS temp dir (ephemeral, but always writable).
	return filepath.Join(os.TempDir(), "pocketpaas-data")
}
