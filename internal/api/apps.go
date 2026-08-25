package api

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/uuid"

	"apphive/internal/auth"
	"apphive/internal/registry"
	"apphive/internal/store"
)

// ---- Auth ----

// failedLoginDelay is the per-attempt delay on failure. Combined with
// LoginLockedOut/RecordLoginFailure this slows parallel brute-force guessing:
// the delay caps per-request throughput and lockouts block repeat offenders.
const failedLoginDelay = 500 * time.Millisecond

func (srv *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	token := auth.CSRFCookieValue(r)
	if token == "" {
		var err error
		token, err = auth.RandomToken()
		if err != nil {
			http.Error(w, "session error", http.StatusInternalServerError)
			return
		}
		auth.SetCSRFCookie(w, token, isSecureRequest(r))
	}
	srv.render(w, "login.html", map[string]any{"Error": "", "CsrfToken": token})
}

func (srv *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if !auth.ValidLoginCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	key := clientIP(r)
	if wait, locked := srv.auth.LoginLockedOut(key); locked {
		srv.renderLogin(w, r, "Too many failed attempts — try again in "+wait.String()+".")
		return
	}
	password := r.FormValue("password")
	if !srv.auth.CheckPassword(password) {
		srv.auth.RecordLoginFailure(key)
		time.Sleep(failedLoginDelay)
		srv.renderLogin(w, r, "Invalid password.")
		return
	}
	srv.auth.RecordLoginSuccess(key)
	token, err := srv.auth.CreateSession()
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	secure := isSecureRequest(r)
	auth.SetCookie(w, token, secure)
	// The readable CSRF cookie carries the session-bound token so JS forms
	// and HTMX can submit it; validation compares against the HMAC, not the
	// cookie, so a planted cookie alone cannot forge a valid pair.
	auth.SetCSRFCookie(w, srv.auth.CSRFForSession(token), secure)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderLogin re-renders the login page with an error, refreshing the CSRF token.
func (srv *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	srv.render(w, "login.html", map[string]any{"Error": errMsg, "CsrfToken": auth.CSRFCookieValue(r)})
}

func (srv *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if token := auth.TokenFromRequest(r); token != "" {
		srv.auth.DeleteSession(token)
	}
	auth.ClearCookies(w)
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
	srv.manager.StartHealthPinger(app)
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
	srv.manager.StartHealthPinger(app)
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
	// Hold the app lock for the whole delete: no pull can be extracting
	// (postPullApp holds the same lock) and no start can slip in between the
	// IsRunning check and the store delete (leaving an unmanaged ghost process).
	unlock, ok := srv.manager.TryLockApp(app.ID)
	if !ok {
		http.Error(w, "another operation is in progress for this app (pull/start/stop) — try again shortly", http.StatusConflict)
		return
	}
	defer unlock()

	if srv.manager.IsRunning(app.ID) {
		// We already hold the app lock — Stop would deadlock on TryLockApp.
		_ = srv.manager.StopLocked(app.ID)
	}
	// Clean up in-memory manager state (log buffers, health map, pingers).
	srv.manager.RemoveApp(app.ID)

	// Remove extracted rootfs from disk — verified to stay under dataDir/apps
	// (imported app IDs are validated, but never trust a join with user data).
	appDir := filepath.Join(srv.dataDir, "apps", app.ID)
	if !hasPathPrefix(appDir, filepath.Join(srv.dataDir, "apps")) {
		log.Printf("refusing to remove path outside apps dir: %s (app %s)", appDir, app.ID)
	} else {
		os.RemoveAll(appDir)
	}

	if err := srv.store.Delete(app.ID); err != nil {
		srv.serverError(w, err)
		return
	}
	w.Header().Set("HX-Redirect", "/")
	w.WriteHeader(http.StatusOK)
}

// hasPathPrefix reports whether child is strictly contained in parent.
func hasPathPrefix(child, parent string) bool {
	c, p := filepath.Clean(child), filepath.Clean(parent)
	return c != p && strings.HasPrefix(c+string(filepath.Separator), p+string(filepath.Separator))
}

// ---- Pull ----

// pullTimeout is the maximum time allowed for a single image pull+extract.
const pullTimeout = 30 * time.Minute

// sendPullLine sends a progress line without ever blocking: if the channel is
// full (e.g. no SSE client is consuming, as in pull-all), the line is dropped
// rather than hanging the pull goroutine forever — which previously leaked
// the pull slot and wedged the app at "pull already in progress".
func sendPullLine(ch chan<- string, msg string) {
	select {
	case ch <- msg:
	default:
		log.Printf("pull: dropped progress line (consumer not draining)")
	}
}

// extractAndSwap extracts img into a staging directory next to destDir and,
// only on success, swaps it into place. A failed or timed-out extract leaves
// the previous working rootfs untouched instead of clobbering it with a
// half-written mix of old and new layers.
func extractAndSwap(ctx context.Context, img v1.Image, destDir string, ch chan<- string) error {
	staging := destDir + ".staging"
	old := destDir + ".old"
	os.RemoveAll(staging)

	if err := registry.Extract(ctx, img, staging, ch); err != nil {
		os.RemoveAll(staging)
		return err
	}

	// Publish: move the previous rootfs aside, move staging into place,
	// then drop the old tree. Rename within the same directory is atomic.
	os.RemoveAll(old)
	hadOld := false
	if _, err := os.Stat(destDir); err == nil {
		if err := os.Rename(destDir, old); err == nil {
			hadOld = true
		}
	}
	if err := os.Rename(staging, destDir); err != nil {
		if hadOld {
			_ = os.Rename(old, destDir) // restore the previous rootfs
		}
		return fmt.Errorf("promote staged rootfs: %w", err)
	}
	os.RemoveAll(old)
	return nil
}

// runPull executes a full pull+extract cycle for one app, sending progress to ch.
// It is shared between single-app and pull-all flows.
func (srv *Server) runPull(appID, imageRef, destDir string, existingEP, existingCMD []string, existingWorkDir string, existingPort int, ch chan<- string) {
	var creds *registry.Credentials
	if srv.regCreds != nil && srv.regCreds.Username != "" {
		creds = srv.regCreds
	}

	ctx, cancel := context.WithTimeout(srv.shutdownCtx, pullTimeout)
	defer cancel()

	img, err := registry.Pull(ctx, imageRef, creds, ch)
	if err != nil {
		sendPullLine(ch, "ERROR: "+err.Error())
		_ = srv.store.UpdateStatus(appID, store.StatusError)
		return
	}

	if err := extractAndSwap(ctx, img, destDir, ch); err != nil {
		sendPullLine(ch, "ERROR: "+err.Error())
		_ = srv.store.UpdateStatus(appID, store.StatusError)
		return
	}

	// Apply image config defaults only when the user has no manual override.
	ep, cmd, workDir, port, cfgErr := registry.ImageConfig(img)
	if cfgErr != nil {
		log.Printf("warn: could not read image config for %s: %v", appID, cfgErr)
	} else {
		var detected []string
		if len(existingEP) == 0 && len(existingCMD) == 0 && (len(ep) > 0 || len(cmd) > 0) {
			_ = srv.store.UpdateEntrypointCmd(appID, ep, cmd)
			if len(ep) > 0 {
				detected = append(detected, fmt.Sprintf("entrypoint=%v", ep))
			}
			if len(cmd) > 0 {
				detected = append(detected, fmt.Sprintf("cmd=%v", cmd))
			}
		}
		if existingWorkDir == "" && workDir != "" {
			_ = srv.store.UpdateWorkDir(appID, workDir)
			detected = append(detected, fmt.Sprintf("workdir=%s", workDir))
		}
		if existingPort == 0 && port > 0 {
			_ = srv.store.UpdateExposedPort(appID, port)
			detected = append(detected, fmt.Sprintf("port=%d", port))
		}
		if len(detected) > 0 {
			sendPullLine(ch, "Auto-configured from image: "+strings.Join(detected, ", "))
		}
	}

	_ = srv.store.UpdateRootfsPath(appID, destDir)
	_ = srv.store.UpdateStatus(appID, store.StatusStopped)
	sendPullLine(ch, "DONE")
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
	// Atomically acquire the pull slot AND the per-app operation lock (held
	// by the pull goroutine until ClosePullChannel), so start/stop/delete on
	// the same app cannot interleave with the extraction.
	ch, release, ok := srv.manager.TryStartPull(app.ID)
	if !ok {
		http.Error(w, "pull already in progress", http.StatusConflict)
		return
	}
	// Recheck under the lock we now hold — a start that raced us above has
	// either finished by now or is blocked on the app lock.
	if srv.manager.IsRunning(app.ID) {
		srv.manager.ClosePullChannel(app.ID)
		release()
		http.Error(w, "stop the app before pulling a new image", http.StatusConflict)
		return
	}

	destDir := filepath.Join(srv.dataDir, "apps", app.ID, "rootfs")
	if !hasPathPrefix(destDir, filepath.Join(srv.dataDir, "apps")) {
		srv.manager.ClosePullChannel(app.ID)
		release()
		http.Error(w, "invalid app id", http.StatusBadRequest)
		return
	}

	_ = srv.store.UpdateStatus(app.ID, store.StatusPulling)

	go func() {
		defer srv.manager.ClosePullChannel(app.ID)
		defer release()
		srv.runPull(app.ID, app.ImageRef, destDir, app.Entrypoint, app.Command, app.WorkDir, app.ExposedPort, ch)
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
		// Also emit the done event so the page reloads and clears the
		// "pulling" state instead of spinning on EventSource reconnects.
		fmt.Fprintf(w, "event: done\ndata: done\n\n")
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
		// Atomically acquire the pull slot + app op lock; skip busy apps,
		// and recheck running under the lock (a start may have raced us).
		ch, release, ok := srv.manager.TryStartPull(app.ID)
		if !ok {
			continue
		}
		if srv.manager.IsRunning(app.ID) {
			srv.manager.ClosePullChannel(app.ID)
			release()
			continue
		}
		destDir := filepath.Join(srv.dataDir, "apps", app.ID, "rootfs")
		if !hasPathPrefix(destDir, filepath.Join(srv.dataDir, "apps")) {
			srv.manager.ClosePullChannel(app.ID)
			release()
			continue
		}
		_ = srv.store.UpdateStatus(app.ID, store.StatusPulling)
		go func(a *store.App, dest string, ch chan string, release func()) {
			defer srv.manager.ClosePullChannel(a.ID)
			defer release()
			srv.runPull(a.ID, a.ImageRef, dest, a.Entrypoint, a.Command, a.WorkDir, a.ExposedPort, ch)
		}(app, destDir, ch, release)
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
	var errs []string
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

	// Entrypoint/command: if the form submitted the field, use it exactly
	// (blank means clear). If an API client omits the field on update, preserve it.
	if _, ok := r.Form["entrypoint"]; ok {
		app.Entrypoint = strings.Fields(strings.TrimSpace(r.FormValue("entrypoint")))
	} else if existing != nil {
		app.Entrypoint = existing.Entrypoint
	}
	if _, ok := r.Form["command"]; ok {
		app.Command = strings.Fields(strings.TrimSpace(r.FormValue("command")))
	} else if existing != nil {
		app.Command = existing.Command
	}

	portStr := strings.TrimSpace(r.FormValue("exposed_port"))
	if portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil {
			if p < 1 || p > 65535 {
				errs = append(errs, "Exposed port must be between 1 and 65535.")
			} else {
				app.ExposedPort = p
			}
		} else {
			errs = append(errs, "Exposed port must be a number.")
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

	if app.Name == "" {
		errs = append(errs, "Name is required.")
	}
	if app.ImageRef == "" {
		errs = append(errs, "Image reference is required.")
	}
	if app.HealthEndpoint != "" && !strings.HasPrefix(app.HealthEndpoint, "/") {
		errs = append(errs, "Health check path must start with '/' (e.g. /health).")
	}

	return app, errs
}

func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// clientIP identifies the client for login rate limiting. The first
// X-Forwarded-For entry is preferred because ngrok (and other reverse
// proxies) terminate at their edge; without it every tunneled user would
// share one bucket. Behind untrusted proxies XFF can be spoofed to dodge
// lockout — the per-request failure delay remains as a floor.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if ip, _, _ := strings.Cut(xff, ","); strings.TrimSpace(ip) != "" {
			return strings.TrimSpace(ip)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
