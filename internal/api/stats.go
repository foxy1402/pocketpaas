package api

import (
	"encoding/json"
	"net/http"
)

// getStats returns live CPU % and RAM usage as JSON.
// Called by the dashboard stats bar every 5 seconds via fetch().
func (srv *Server) getStats(w http.ResponseWriter, r *http.Request) {
	s := srv.sampler.Get()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"cpu_pct":      s.CPUPercent,
		"ram_used_mb":  s.RAMUsedMB,
		"ram_total_mb": s.RAMTotalMB,
	})
}
