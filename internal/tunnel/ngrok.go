// Package tunnel provides an optional ngrok HTTP tunnel.
// It is enabled only when NGROK_AUTHTOKEN is set; otherwise all methods are no-ops.
package tunnel

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync/atomic"

	ngroklib "golang.ngrok.com/ngrok/v2"
)

// Manager manages an optional ngrok HTTP tunnel.
// All methods are safe to call on a nil pointer or zero value.
type Manager struct {
	url atomic.Value // stores string
}

// URL returns the public ngrok URL (e.g. "https://abc123.ngrok-free.app"),
// or "" when no tunnel is active yet.
func (m *Manager) URL() string {
	if m == nil {
		return ""
	}
	if v := m.url.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// Start begins an ngrok HTTP tunnel in the background, serving handler on
// the public URL AND on the local port (the caller keeps listening locally).
//
//   - authtoken: ngrok auth token (required).
//   - domain: optional static hostname, e.g. "your-app.ngrok-free.app".
//     Leave empty to let ngrok assign a random URL.
//
// Returns immediately; the tunnel connects asynchronously and the URL is
// available via URL() once connected. Logs all status changes.
func (m *Manager) Start(ctx context.Context, handler http.Handler, authtoken, domain string) {
	go func() {
		agent, err := ngroklib.NewAgent(ngroklib.WithAuthtoken(authtoken))
		if err != nil {
			log.Printf("ngrok: create agent: %v", err)
			return
		}

		var opts []ngroklib.EndpointOption
		if domain != "" {
			// Accept bare hostname or full https:// URL.
			if !strings.HasPrefix(domain, "https://") && !strings.HasPrefix(domain, "http://") {
				domain = "https://" + domain
			}
			opts = append(opts, ngroklib.WithURL(domain))
		}

		ln, err := agent.Listen(ctx, opts...)
		if err != nil {
			log.Printf("ngrok: start tunnel: %v", err)
			return
		}

		publicURL := ln.URL().String()
		m.url.Store(publicURL)
		log.Printf("ngrok: tunnel active → %s", publicURL)
		log.Printf("ngrok: dashboard is reachable at %s  (login with DASHBOARD_PASSWORD)", publicURL)

		if err := http.Serve(ln, handler); err != nil && ctx.Err() == nil {
			log.Printf("ngrok: tunnel closed: %v", err)
		}
		m.url.Store("")
	}()
}
