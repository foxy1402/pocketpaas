package api

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
	filename := fmt.Sprintf("apphive-export-%s.json", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Write(data)
}

func (srv *Server) postImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
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
	if err := r.ParseMultipartForm(10 << 20); err != nil {
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

	// Flush history so a late-connecting client sees what already happened.
	for _, line := range ds.Log.Lines() {
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

	sub := ds.Log.Subscribe()
	defer ds.Log.Unsubscribe(sub)

	for {
		select {
		case line, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", escapeLine(line))
			flusher.Flush()
		case <-ds.Done:
			fmt.Fprintf(w, "event: done\ndata: done\n\n")
			flusher.Flush()
			return
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

		img, err := registry.Pull(app.ImageRef, creds, pullCh)
		if err != nil {
			close(pullCh)
			<-pullRelayed
			dlog.Write("│  ✗ Pull failed: " + err.Error())
			dlog.Write("└─ FAILED")
			dlog.Write("")
			_ = srv.store.UpdateStatus(app.ID, store.StatusError)
			continue
		}
		if err := registry.Extract(img, destDir, pullCh); err != nil {
			close(pullCh)
			<-pullRelayed
			dlog.Write("│  ✗ Extract failed: " + err.Error())
			dlog.Write("└─ FAILED")
			dlog.Write("")
			_ = srv.store.UpdateStatus(app.ID, store.StatusError)
			continue
		}
		close(pullCh)
		<-pullRelayed

		// Apply image config defaults only when the user has no overrides.
		ep, cmd, workDir, cfgErr := registry.ImageConfig(img)
		if cfgErr != nil {
			log.Printf("warn: image config for %s: %v", app.ID, cfgErr)
		} else {
			if len(app.Entrypoint) == 0 && len(ep) > 0 {
				_ = srv.store.UpdateEntrypointCmd(app.ID, ep, cmd)
			}
			if app.WorkDir == "" && workDir != "" {
				_ = srv.store.UpdateWorkDir(app.ID, workDir)
			}
		}
		_ = srv.store.UpdateRootfsPath(app.ID, destDir)
		_ = srv.store.UpdateStatus(app.ID, store.StatusStopped)

		// ── Start ──
		dlog.Write("│  Starting app…")
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

		// ── Prune ── (always prune to free ephemeral storage for next app)
		dlog.Write("│  Pruning rootfs…")
		if err := os.RemoveAll(destDir); err != nil {
			dlog.Write("│  ✗ Prune error: " + err.Error())
		} else {
			_ = srv.store.UpdateRootfsPath(app.ID, "")
			dlog.Write("│  Disk freed ✓")
		}

		dlog.Write("└─ Done")
		dlog.Write("")
	}

	dlog.Write("═══════════════════════════════════")
	dlog.Write("Sequential deploy complete!")
}
