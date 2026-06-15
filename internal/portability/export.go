package portability

import (
	"encoding/json"
	"time"

	"apphive/internal/store"
)

const exportVersion = 1

// ExportFile is the top-level JSON structure for export/import.
type ExportFile struct {
	Version    int          `json:"version"`
	ExportedAt time.Time    `json:"exported_at"`
	Apps       []ExportedApp `json:"apps"`
}

// ExportedApp holds all data needed to reconstruct an app record.
// RootfsPath is intentionally excluded — it is a host-specific path that must
// be recreated by pulling the image on the new host.
type ExportedApp struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	ImageRef        string         `json:"image_ref"`
	Entrypoint      []string       `json:"entrypoint"`
	Command         []string       `json:"command"`
	WorkDir         string         `json:"work_dir"`
	EnvVars         []store.EnvVar `json:"env_vars"`
	AutoStart       bool           `json:"auto_start"`
	ExposedPort     int            `json:"exposed_port"`
	HealthEndpoint  string         `json:"health_endpoint"`
	PruneAfterStart bool           `json:"prune_after_start"`
}

// Export serializes all apps from the store into an ExportFile JSON payload.
func Export(s *store.AppStore) ([]byte, error) {
	apps, err := s.List()
	if err != nil {
		return nil, err
	}

	ef := ExportFile{
		Version:    exportVersion,
		ExportedAt: time.Now().UTC(),
		Apps:       make([]ExportedApp, 0, len(apps)),
	}

	for _, a := range apps {
		ef.Apps = append(ef.Apps, ExportedApp{
			ID:              a.ID,
			Name:            a.Name,
			ImageRef:        a.ImageRef,
			Entrypoint:      a.Entrypoint,
			Command:         a.Command,
			WorkDir:         a.WorkDir,
			EnvVars:         a.EnvVars,
			AutoStart:       a.AutoStart,
			ExposedPort:     a.ExposedPort,
			HealthEndpoint:  a.HealthEndpoint,
			PruneAfterStart: a.PruneAfterStart,
		})
	}

	return json.MarshalIndent(ef, "", "  ")
}
