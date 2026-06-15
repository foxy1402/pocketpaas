package api

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"time"

	"apphive/internal/auth"
	"apphive/internal/registry"
	"apphive/internal/runtime"
	"apphive/internal/store"
	"apphive/internal/sysinfo"
	"apphive/internal/tunnel"
)

// Server holds all shared dependencies for the HTTP handlers.
type Server struct {
	store    *store.AppStore
	manager  *runtime.Manager
	auth     *auth.Manager
	pages    map[string]*template.Template
	dataDir  string
	regCreds *registry.Credentials
	sampler  *sysinfo.Sampler
	tunnel   *tunnel.Manager
	innerMux *http.ServeMux // protected routes — built once at startup
}

// NewServer constructs the Server from its dependencies.
func NewServer(
	s *store.AppStore,
	m *runtime.Manager,
	a *auth.Manager,
	tmplFS embed.FS,
	dataDir string,
	regCreds *registry.Credentials,
	sampler *sysinfo.Sampler,
	tunMgr *tunnel.Manager,
) *Server {
	funcs := template.FuncMap{
		"statusClass": statusClass,
		"maskValue":   maskValue,
		"formatTime":  formatTime,
		// ngrokURL returns the live public URL when a tunnel is active, or "".
		"ngrokURL": func() string { return tunMgr.URL() },
	}

	// Parse each page together with the layout so {{block}} inheritance works
	// correctly — each pair is an independent template set.
	layoutPath := "web/templates/layout.html"
	pageNames := []string{"apps.html", "app_detail.html", "app_form.html", "portability.html", "deploy_progress.html"}

	pages := make(map[string]*template.Template, len(pageNames)+1)
	for _, p := range pageNames {
		t := template.Must(
			template.New(p).Funcs(funcs).ParseFS(tmplFS, layoutPath, "web/templates/"+p),
		)
		pages[p] = t
	}
	// login.html is standalone (no layout).
	pages["login.html"] = template.Must(
		template.New("login.html").Funcs(funcs).ParseFS(tmplFS, "web/templates/login.html"),
	)

	srv := &Server{
		store:    s,
		manager:  m,
		auth:     a,
		pages:    pages,
		dataDir:  dataDir,
		regCreds: regCreds,
		sampler:  sampler,
		tunnel:   tunMgr,
	}
	srv.innerMux = srv.buildProtectedMux()
	return srv
}

// buildProtectedMux creates the mux for authenticated routes (called once at startup).
func (srv *Server) buildProtectedMux() *http.ServeMux {
	inner := http.NewServeMux()

	inner.HandleFunc("GET /{$}", srv.listApps)

	inner.HandleFunc("GET /apps/new", srv.getNewApp)
	inner.HandleFunc("POST /apps", srv.postCreateApp)
	inner.HandleFunc("POST /apps/pull-all", srv.postPullAll)

	inner.HandleFunc("GET /apps/{id}", srv.getAppDetail)
	inner.HandleFunc("GET /apps/{id}/edit", srv.getEditApp)
	inner.HandleFunc("POST /apps/{id}", srv.postUpdateApp)
	inner.HandleFunc("DELETE /apps/{id}", srv.deleteApp)
	inner.HandleFunc("POST /apps/{id}/pull", srv.postPullApp)
	inner.HandleFunc("GET /apps/{id}/pull-progress", srv.getPullProgress)
	inner.HandleFunc("POST /apps/{id}/start", srv.postStartApp)
	inner.HandleFunc("POST /apps/{id}/stop", srv.postStopApp)
	inner.HandleFunc("POST /apps/{id}/restart", srv.postRestartApp)
	inner.HandleFunc("GET /apps/{id}/logs", srv.getLogsSSE)
	inner.HandleFunc("GET /apps/{id}/health", srv.getHealthStatus)

	inner.HandleFunc("GET /portability", srv.getPortability)
	inner.HandleFunc("GET /portability/export", srv.getExport)
	inner.HandleFunc("POST /portability/import", srv.postImport)
	inner.HandleFunc("POST /portability/sequential-deploy", srv.postSequentialDeploy)
	inner.HandleFunc("GET /portability/deploy-progress", srv.getDeployProgress)
	inner.HandleFunc("GET /portability/deploy-stream", srv.getDeployStream)

	inner.HandleFunc("GET /api/stats", srv.getStats)

	return inner
}

// RegisterRoutes attaches all routes to mux.
func (srv *Server) RegisterRoutes(mux *http.ServeMux, staticFS embed.FS) {
	// Serve static files.
	sub, err := fs.Sub(staticFS, "web/static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))

	// Public routes.
	mux.HandleFunc("GET /login", srv.getLogin)
	mux.HandleFunc("POST /login", srv.postLogin)
	mux.HandleFunc("GET /logout", srv.getLogout)

	// Protected routes wrapped with auth middleware.
	mux.Handle("/", srv.auth.Middleware(srv.innerMux))
}

// Template helpers.

func statusClass(s store.AppStatus) string {
	switch s {
	case store.StatusRunning:
		return "badge-running"
	case store.StatusPulling:
		return "badge-pulling"
	case store.StatusCrashed:
		return "badge-crashed"
	case store.StatusError:
		return "badge-error"
	default:
		return "badge-stopped"
	}
}

func maskValue(s string) string {
	if len(s) == 0 {
		return ""
	}
	return "••••••••"
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Format("2006-01-02 15:04:05 UTC")
}
