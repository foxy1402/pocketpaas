package api

import (
	"fmt"
	"net/http"
	"strings"
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

	for {
		select {
		case line, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", escapeLine(line))
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
