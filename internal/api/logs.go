package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// getLogsSSE streams the app's log buffer as Server-Sent Events.
// On connect it flushes the full ring buffer, then streams new lines live.
func (srv *Server) getLogsSSE(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	buf := srv.manager.LogBuffer(id)

	// Send buffered history.
	for _, line := range buf.Lines() {
		fmt.Fprintf(w, "data: %s\n\n", escapeLine(line))
		flusher.Flush()
	}

	// Subscribe to new lines.
	sub := buf.Subscribe()
	defer buf.Unsubscribe(sub)

	// Keepalive ticker: writes a comment every 25 s.
	// If the write fails (proxy dropped the connection but didn't signal the context),
	// we return, freeing this goroutine and its subscription.
	ping := time.NewTicker(25 * time.Second)
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

func escapeLine(s string) string {
	// SSE data fields must not contain raw newlines.
	return strings.ReplaceAll(s, "\n", " ")
}
