package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"apphive/internal/portability"
	"apphive/internal/registry"
	"apphive/internal/runtime"
	"apphive/internal/store"
)

func (srv *Server) getPortability(w http.ResponseWriter, r *http.Request) {
	srv.render(w, "portability.html", nil)
}

func (srv *Server) getExport(w http.ResponseWriter, r *http.Request) {
	data, err := portability.Export(srv.store)
	if err != nil {
		srv.serverError(w, err)
		return
	}
	filename := fmt.Sprintf("pocketpaas-export-%s.json", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Write(data)
}

// maxImportBytes caps import-upload body size. Note: ParseMultipartForm's
// argument is only the in-RAM threshold (the rest spills to temp files), so
// it is NOT a size limit — MaxBytesReader provides the real cap.
const maxImportBytes = portability.MaxExportBytes

func (srv *Server) postImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "request too large", http.StatusBadRequest)
		return
	}

	f, _, err := r.FormFile("import_file")
	if err != nil {
		http.Error(w, "file required", http.StatusBadRequest)
		return
	}
	defer f.Close()

	result, err := portability.Import(f, srv.store)
	if err != nil {
		srv.render(w, "portability.html", map[string]any{
			"ImportError": err.Error(),
		})
		return
	}

	// Start health pingers for apps with health endpoints (deduplication handled by manager).
	apps, _ := srv.store.List()
	for _, app := range apps {
		if app.HealthEndpoint != "" {
			srv.manager.StartHealthPinger(app)
		}
	}

	srv.render(w, "portability.html", map[string]any{
		"ImportResult": result,
	})
}

// ---- Sequential Deploy ----

// postSequentialDeploy parses an export file, imports app configs, then kicks
// off a background goroutine that pulls → starts → prunes each app in order.
func (srv *Server) postSequentialDeploy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBytes)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		http.Error(w, "request too large", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("import_file")
	if err != nil {
		http.Error(w, "file required", http.StatusBadRequest)
		return
	}
	defer f.Close()

	ef, err := portability.ParseExport(f)
	if err != nil {
		http.Error(w, "invalid export file: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Acquire the deploy slot; reject if already running.
	ds := srv.manager.StartDeploy()
	if ds == nil {
		http.Error(w, "a sequential deploy is already in progress", http.StatusConflict)
		return
	}

	// Import all app configs first (idempotent — existing IDs are skipped).
	result, err := portability.ImportParsed(ef, srv.store)
	if err != nil {
		srv.manager.FinishDeploy()
		http.Error(w, "import failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build ordered app ID list preserving the export file's ordering.
	appIDs := make([]string, 0, len(ef.Apps))
	for _, ea := range ef.Apps {
		appIDs = append(appIDs, ea.ID)
	}

	go srv.runSequentialDeploy(ds, appIDs, result)

	http.Redirect(w, r, "/portability/deploy-progress", http.StatusSeeOther)
}

// getDeployProgress renders the deploy progress page.
func (srv *Server) getDeployProgress(w http.ResponseWriter, r *http.Request) {
	srv.render(w, "deploy_progress.html", nil)
}

// getDeployStream streams sequential deploy progress as SSE.
func (srv *Server) getDeployStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	ds := srv.manager.CurrentDeploy()
	if ds == nil {
		fmt.Fprintf(w, "data: No deploy active.\n\n")
		fmt.Fprintf(w, "event: done\ndata: done\n\n")
		flusher.Flush()
		return
	}

	// Atomically grab all history lines and register for future lines so no
	// lines are missed between the snapshot and the subscription.
	history, sub := ds.Log.SubscribeWithHistory()
	defer ds.Log.Unsubscribe(sub)

	for _, line := range history {
		fmt.Fprintf(w, "data: %s\n\n", escapeLine(line))
	}
	flusher.Flush()

	// Already finished? Signal immediately.
	select {
	case <-ds.Done:
		fmt.Fprintf(w, "event: done\ndata: done\n\n")
		flusher.Flush()
		return
	default:
	}

	// Keepalive as a named "ping" event (carries data) so the client can
	// observe it and reset its dead-connection watchdog. A bare ": ping"
	// comment would be invisible to JS.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case line, ok := <-sub:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", escapeLine(line)); err != nil {
				return
			}
			flusher.Flush()
		case <-ds.Done:
			fmt.Fprintf(w, "event: done\ndata: done\n\n")
			flusher.Flush()
			return
		case <-ping.C:
			if _, err := fmt.Fprintf(w, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// runSequentialDeploy is the background goroutine: pull → start → prune for each app.
func (srv *Server) runSequentialDeploy(ds *runtime.DeployState, appIDs []string, result *portability.ImportResult) {
	defer srv.manager.FinishDeploy()

	dlog := ds.Log
	total := len(appIDs)

	dlog.Write(fmt.Sprintf("Starting sequential deploy — %d app(s) queued.", total))
	if result != nil && len(result.Errors) > 0 {
		for _, e := range result.Errors {
			dlog.Write("  import warning: " + e)
		}
	}
	dlog.Write("")

	var creds *registry.Credentials
	if srv.regCreds != nil && srv.regCreds.Username != "" {
		creds = srv.regCreds
	}

	for i, appID := range appIDs {
		app, err := srv.store.Get(appID)
		if err != nil || app == nil {
			dlog.Write(fmt.Sprintf("[%d/%d] SKIP: app %s not found", i+1, total, appID))
			dlog.Write("")
			continue
		}

		dlog.Write(fmt.Sprintf("┌─ [%d/%d] %s", i+1, total, app.Name))
		dlog.Write(fmt.Sprintf("│  Image: %s", app.ImageRef))

		destDir := filepath.Join(srv.dataDir, "apps", app.ID, "rootfs")

		// Acquire the pull slot + app op lock so a manual start/pull/delete on
		// this app cannot interleave with our extract+start sequence.
		_, release, ok := srv.manager.TryStartPull(app.ID)
		if !ok {
			dlog.Write("│  ✗ SKIP: another pull is already in progress for this app")
			dlog.Write("└─ SKIPPED")
			dlog.Write("")
			continue
		}
		// Stop a running app before touching its rootfs — the staging swap
		// renames the rootfs out from under a chrooted process otherwise.
		// (We hold the app lock, so use StopLocked.)
		if srv.manager.IsRunning(app.ID) {
			dlog.Write("│  Stopping running app before re-pull…")
			_ = srv.manager.StopLocked(app.ID)
		}
		_ = srv.store.UpdateStatus(app.ID, store.StatusPulling)

		// ── Pull ──
		dlog.Write("│  Pulling…")
		pullCh := make(chan string, 128)
		pullRelayed := make(chan struct{})
		go func() {
			defer close(pullRelayed)
			for line := range pullCh {
				dlog.Write("│    " + line)
			}
		}()

		// Context covers both manifest fetch AND layer downloads — cancel only
		// after Extract; also aborts cleanly on server shutdown.
		pullCtx, pullCancel := context.WithTimeout(srv.shutdownCtx, 30*time.Minute)
		img, err := registry.Pull(pullCtx, app.ImageRef, creds, pullCh)
		if err != nil {
			pullCancel()
			close(pullCh)
			<-pullRelayed
			srv.manager.ClosePullChannel(app.ID)
			release()
			dlog.Write("│  ✗ Pull failed: " + err.Error())
			dlog.Write("└─ FAILED")
			dlog.Write("")
			_ = srv.store.UpdateStatus(app.ID, store.StatusError)
			continue
		}
		// Extract into a staging dir and swap atomically, so a failed deploy
		// never destroys the previous working rootfs.
		if err := extractAndSwap(pullCtx, img, destDir, pullCh); err != nil {
			pullCancel()
			close(pullCh)
			<-pullRelayed
			srv.manager.ClosePullChannel(app.ID)
			release()
			dlog.Write("│  ✗ Extract failed: " + err.Error())
			dlog.Write("└─ FAILED")
			dlog.Write("")
			_ = srv.store.UpdateStatus(app.ID, store.StatusError)
			continue
		}
		pullCancel()
		close(pullCh)
		<-pullRelayed
		srv.manager.ClosePullChannel(app.ID)
		release()

		// Apply image config defaults only when the user has no overrides.
		ep, cmd, workDir, port, cfgErr := registry.ImageConfig(img)
		if cfgErr != nil {
			log.Printf("warn: image config for %s: %v", app.ID, cfgErr)
		} else {
			var detected []string
			if len(app.Entrypoint) == 0 && len(app.Command) == 0 && (len(ep) > 0 || len(cmd) > 0) {
				_ = srv.store.UpdateEntrypointCmd(app.ID, ep, cmd)
				if len(ep) > 0 {
					detected = append(detected, fmt.Sprintf("entrypoint=%v", ep))
				}
				if len(cmd) > 0 {
					detected = append(detected, fmt.Sprintf("cmd=%v", cmd))
				}
			}
			if app.WorkDir == "" && workDir != "" {
				_ = srv.store.UpdateWorkDir(app.ID, workDir)
				detected = append(detected, fmt.Sprintf("workdir=%s", workDir))
			}
			if app.ExposedPort == 0 && port > 0 {
				_ = srv.store.UpdateExposedPort(app.ID, port)
				detected = append(detected, fmt.Sprintf("port=%d", port))
			}
			if len(detected) > 0 {
				dlog.Write("│  Auto-configured: " + strings.Join(detected, ", "))
			}
		}
		_ = srv.store.UpdateRootfsPath(app.ID, destDir)
		_ = srv.store.UpdateStatus(app.ID, store.StatusStopped)

		// ── Prune ── BEFORE start: caches/docs/temp are removed while nothing
		// is running, and /tmp is recreated empty (pruning a live app could
		// delete its runtime temp files).
		dlog.Write("│  Pruning caches…")
		if err := runtime.PruneRootfsKeepDNS(destDir); err != nil {
			dlog.Write("│  ✗ Prune error: " + err.Error())
		} else {
			dlog.Write("│  Cache/docs pruned ✓")
		}

		// ── Start ──
		dlog.Write("│  Starting app…")
		if srv.manager.IsRunning(app.ID) {
			_ = srv.manager.Stop(app.ID)
		}
		if err := srv.manager.Start(app.ID); err != nil {
			dlog.Write("│  ✗ Start failed: " + err.Error())
		} else {
			// Brief stabilisation window — if the process dies quickly it likely crashed.
			time.Sleep(5 * time.Second)
			if srv.manager.IsRunning(app.ID) {
				dlog.Write("│  ✓ Running")
			} else {
				dlog.Write("│  ⚠ Exited shortly after start — check app logs")
			}
		}

		dlog.Write("└─ Done")
		dlog.Write("")
	}

	dlog.Write("═══════════════════════════════════")
	dlog.Write("Sequential deploy complete!")
}
