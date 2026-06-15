package api

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"apphive/internal/auth"
	"apphive/internal/registry"
	"apphive/internal/store"
)

// ---- Auth ----

func (srv *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	srv.render(w, "login.html", map[string]any{"Error": ""})
}

func (srv *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	password := r.FormValue("password")
	if !srv.auth.CheckPassword(password) {
		srv.render(w, "login.html", map[string]any{"Error": "Invalid password."})
		return
	}
	token, err := srv.auth.CreateSession()
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	auth.SetCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (srv *Server) getLogout(w http.ResponseWriter, r *http.Request) {
	if token := auth.TokenFromRequest(r); token != "" {
		srv.auth.DeleteSession(token)
	}
	auth.ClearCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- App List ----

func (srv *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := srv.store.List()
	if err != nil {
		srv.serverError(w, err)
		return
	}
	srv.render(w, "apps.html", map[string]any{
		"Apps":    apps,
		"Manager": srv.manager,
	})
}

// ---- Create / Edit ----

func (srv *Server) getNewApp(w http.ResponseWriter, r *http.Request) {
	srv.render(w, "app_form.html", map[string]any{
		"App":    &store.App{},
		"IsNew":  true,
		"Errors": nil,
	})
}

func (srv *Server) postCreateApp(w http.ResponseWriter, r *http.Request) {
	app, errs := srv.parseAppForm(r, nil)
	if len(errs) > 0 {
		srv.render(w, "app_form.html", map[string]any{"App": app, "IsNew": true, "Errors": errs})
		return
	}
	app.ID = uuid.New().String()
	app.Status = store.StatusStopped
	if err := srv.store.Create(app); err != nil {
		srv.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (srv *Server) getEditApp(w http.ResponseWriter, r *http.Request) {
	app := srv.mustGetApp(w, r)
	if app == nil {
		return
	}
	srv.render(w, "app_form.html", map[string]any{"App": app, "IsNew": false, "Errors": nil})
}

func (srv *Server) postUpdateApp(w http.ResponseWriter, r *http.Request) {
	existing := srv.mustGetApp(w, r)
	if existing == nil {
		return
	}
	app, errs := srv.parseAppForm(r, existing)
	if len(errs) > 0 {
		srv.render(w, "app_form.html", map[string]any{"App": app, "IsNew": false, "Errors": errs})
		return
	}
	if err := srv.store.Update(app); err != nil {
		srv.serverError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/apps/%s", app.ID), http.StatusSeeOther)
}

// ---- Detail ----

func (srv *Server) getAppDetail(w http.ResponseWriter, r *http.Request) {
	app := srv.mustGetApp(w, r)
	if app == nil {
		return
	}
	srv.render(w, "app_detail.html", map[string]any{
		"App":          app,
		"Manager":      srv.manager,
		"HealthStatus": srv.manager.HealthStatus(app.ID).String(),
	})
}

// ---- Delete ----

func (srv *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	app := srv.mustGetApp(w, r)
	if app == nil {
		return
	}
	if srv.manager.IsRunning(app.ID) {
		_ = srv.manager.Stop(app.ID)
	}
	// Clean up in-memory manager state (log buffers, health map, pingers).
	srv.manager.RemoveApp(app.ID)
	// Remove extracted rootfs from disk.
	os.RemoveAll(filepath.Join(srv.dataDir, "apps", app.ID))

	if err := srv.store.Delete(app.ID); err != nil {
		srv.serverError(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

// ---- Pull ----

// pullTimeout is the maximum time allowed for a single image pull+extract.
const pullTimeout = 30 * time.Minute

// runPull executes a full pull+extract cycle for one app, sending progress to ch.
// It is shared between single-app and pull-all flows.
func (srv *Server) runPull(appID, imageRef, destDir string, existingEP, existingCMD []string, existingWorkDir string, ch chan<- string) {
	var creds *registry.Credentials
	if srv.regCreds != nil && srv.regCreds.Username != "" {
		creds = srv.regCreds
	}

	ctx, cancel := context.WithTimeout(context.Background(), pullTimeout)
	defer cancel()

	img, err := registry.Pull(ctx, imageRef, creds, ch)
	if err != nil {
		ch <- "ERROR: " + err.Error()
		_ = srv.store.UpdateStatus(appID, store.StatusError)
		return
	}

	if err := registry.Extract(img, destDir, ch); err != nil {
		ch <- "ERROR: " + err.Error()
		_ = srv.store.UpdateStatus(appID, store.StatusError)
		return
	}

	// Store image-provided defaults only when the user has no override.
	ep, cmd, workDir, err := registry.ImageConfig(img)
	if err != nil {
		log.Printf("warn: could not read image config for %s: %v", appID, err)
	} else {
		if len(existingEP) == 0 && len(ep) > 0 {
			_ = srv.store.UpdateEntrypointCmd(appID, ep, cmd)
		}
		if existingWorkDir == "" && workDir != "" {
			_ = srv.store.UpdateWorkDir(appID, workDir)
		}
	}

	_ = srv.store.UpdateRootfsPath(appID, destDir)
	_ = srv.store.UpdateStatus(appID, store.StatusStopped)
	ch <- "DONE"
}

func (srv *Server) postPullApp(w http.ResponseWriter, r *http.Request) {
	app := srv.mustGetApp(w, r)
	if app == nil {
		return
	}
	if srv.manager.IsRunning(app.ID) {
		http.Error(w, "stop the app before pulling a new image", http.StatusConflict)
		return
	}
	if srv.manager.PullChannel(app.ID) != nil {
		http.Error(w, "pull already in progress", http.StatusConflict)
		return
	}

	_ = srv.store.UpdateStatus(app.ID, store.StatusPulling)
	destDir := filepath.Join(srv.dataDir, "apps", app.ID, "rootfs")

	go func() {
		ch := srv.manager.OpenPullChannel(app.ID)
		defer srv.manager.ClosePullChannel(app.ID)
		srv.runPull(app.ID, app.ImageRef, destDir, app.Entrypoint, app.Command, app.WorkDir, ch)
	}()

	http.Redirect(w, r, fmt.Sprintf("/apps/%s", app.ID), http.StatusSeeOther)
}

// getPullProgress streams pull progress via SSE.
func (srv *Server) getPullProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Poll briefly for the pull channel to appear (it's opened by the goroutine
	// before we redirect, so this typically resolves immediately).
	var ch chan string
	for i := 0; i < 20; i++ {
		ch = srv.manager.PullChannel(id)
		if ch != nil {
			break
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if ch == nil {
		fmt.Fprintf(w, "data: No active pull.\n\n")
		flusher.Flush()
		return
	}

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case line, ok := <-ch:
			if !ok {
				fmt.Fprintf(w, "event: done\ndata: Pull complete.\n\n")
				flusher.Flush()
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", escapeLine(line)); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// postPullAll triggers a pull for every app in stopped/error/crashed state.
func (srv *Server) postPullAll(w http.ResponseWriter, r *http.Request) {
	apps, err := srv.store.List()
	if err != nil {
		srv.serverError(w, err)
		return
	}
	for _, app := range apps {
		if app.Status != store.StatusStopped &&
			app.Status != store.StatusError &&
			app.Status != store.StatusCrashed {
			continue
		}
		if srv.manager.PullChannel(app.ID) != nil {
			continue
		}
		_ = srv.store.UpdateStatus(app.ID, store.StatusPulling)
		destDir := filepath.Join(srv.dataDir, "apps", app.ID, "rootfs")
		go func(a *store.App, dest string) {
			ch := srv.manager.OpenPullChannel(a.ID)
			defer srv.manager.ClosePullChannel(a.ID)
			srv.runPull(a.ID, a.ImageRef, dest, a.Entrypoint, a.Command, a.WorkDir, ch)
		}(app, destDir)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- Start / Stop / Restart ----

func (srv *Server) postStartApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := srv.manager.Start(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/apps/%s", id), http.StatusSeeOther)
}

func (srv *Server) postStopApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := srv.manager.Stop(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/apps/%s", id), http.StatusSeeOther)
}

func (srv *Server) postRestartApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := srv.manager.Restart(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/apps/%s", id), http.StatusSeeOther)
}

// ---- Health ----

func (srv *Server) getHealthStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state := srv.manager.HealthStatus(id)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":%q}`, state.String())
}

// ---- Helpers ----

func (srv *Server) mustGetApp(w http.ResponseWriter, r *http.Request) *store.App {
	id := r.PathValue("id")
	app, err := srv.store.Get(id)
	if err != nil {
		srv.serverError(w, err)
		return nil
	}
	if app == nil {
		http.NotFound(w, r)
		return nil
	}
	return app
}

func (srv *Server) render(w http.ResponseWriter, name string, data any) {
	t, ok := srv.pages[name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error (%s): %v", name, err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

func (srv *Server) serverError(w http.ResponseWriter, err error) {
	log.Printf("server error: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (srv *Server) parseAppForm(r *http.Request, existing *store.App) (*store.App, []string) {
	if err := r.ParseForm(); err != nil {
		return &store.App{}, []string{"Could not parse form."}
	}

	app := &store.App{}
	if existing != nil {
		// Preserve fields that are set by the pull process, not by the user form.
		app.ID = existing.ID
		app.Status = existing.Status
		app.CreatedAt = existing.CreatedAt
		app.WorkDir = existing.WorkDir
		app.RootfsPath = existing.RootfsPath
	}

	app.Name = strings.TrimSpace(r.FormValue("name"))
	app.ImageRef = strings.TrimSpace(r.FormValue("image_ref"))
	app.HealthEndpoint = strings.TrimSpace(r.FormValue("health_endpoint"))

	// Entrypoint/command: preserve existing when field is blank (user didn't clear it)
	// but allow explicit clearing by submitting a space character.
	if ep := strings.TrimSpace(r.FormValue("entrypoint")); ep != "" {
		app.Entrypoint = strings.Fields(ep)
	} else if existing != nil {
		app.Entrypoint = existing.Entrypoint
	}
	if cmd := strings.TrimSpace(r.FormValue("command")); cmd != "" {
		app.Command = strings.Fields(cmd)
	} else if existing != nil {
		app.Command = existing.Command
	}

	portStr := strings.TrimSpace(r.FormValue("exposed_port"))
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil {
			app.ExposedPort = p
		}
	}
	app.AutoStart = r.FormValue("auto_start") == "on"
	app.PruneAfterStart = r.FormValue("prune_after_start") == "on"

	// Env vars: parallel arrays from form.
	keys := r.Form["env_key"]
	vals := r.Form["env_value"]
	for i, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		app.EnvVars = append(app.EnvVars, store.EnvVar{Key: k, Value: v})
	}

	var errs []string
	if app.Name == "" {
		errs = append(errs, "Name is required.")
	}
	if app.ImageRef == "" {
		errs = append(errs, "Image reference is required.")
	}

	return app, errs
}

