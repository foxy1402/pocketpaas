package main

import (
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"apphive/internal/api"
	"apphive/internal/auth"
	"apphive/internal/registry"
	"apphive/internal/runtime"
	"apphive/internal/store"
	"apphive/internal/sysinfo"
)

//go:embed all:web
var webFS embed.FS

// Build-time variables injected by Dockerfile ldflags (-X main.buildTime=... -X main.revision=...).
var (
	buildTime string
	revision  string
)

func main() {
	password := os.Getenv("DASHBOARD_PASSWORD")
	if password == "" {
		log.Fatal("DASHBOARD_PASSWORD environment variable is required")
	}

	dataDir := envOrDefault("DATA_DIR", "/data")
	port := envOrDefault("PORT", "8080")

	regCreds := &registry.Credentials{
		Username: os.Getenv("REGISTRY_USER"),
		Password: os.Getenv("REGISTRY_PASSWORD"),
	}

	// Ensure data directory exists.
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("create data dir %s: %v", dataDir, err)
	}

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

	// Bootstrap runtime manager.
	mgr := runtime.NewManager(appStore, dataDir)

	// Auth manager.
	authMgr := auth.NewManager(password)

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
	srv := api.NewServer(appStore, mgr, authMgr, webFS, dataDir, regCreds, sampler)
	srv.RegisterRoutes(mux, webFS)

	// Auto-start apps that have the flag set (only meaningful with persistent storage).
	autoStartApps(appStore, mgr)

	// Start health pingers for apps that have a health endpoint.
	startHealthPingers(appStore, mgr)

	addr := fmt.Sprintf(":%s", port)
	rev := revision
	if rev == "" {
		rev = "dev"
	}
	log.Printf("pocketpaas %s (built %s) listening on %s (data: %s)", rev, buildTime, addr, dataDir)
	if err := http.ListenAndServe(addr, mux); err != nil {
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
