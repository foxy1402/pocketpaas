package portability

import (
	"encoding/json"
	"fmt"
	"io"

	"apphive/internal/store"
)

// ParseExport reads and validates a JSON export from r without importing to the store.
func ParseExport(r io.Reader) (*ExportFile, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var ef ExportFile
	if err := json.Unmarshal(data, &ef); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if ef.Version != exportVersion {
		return nil, fmt.Errorf("unsupported export version %d (expected %d)", ef.Version, exportVersion)
	}
	return &ef, nil
}

// ImportParsed restores apps from an already-parsed ExportFile. Apps with
// existing IDs are skipped. This is useful when you need to parse first
// (e.g. for validation) then import.
func ImportParsed(ef *ExportFile, s *store.AppStore) (*ImportResult, error) {
	result := &ImportResult{}
	for _, ea := range ef.Apps {
		existing, err := s.Get(ea.ID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", ea.Name, err))
			continue
		}
		if existing != nil {
			result.Skipped++
			continue
		}
		app := exportedToApp(ea)
		if err := s.Create(app); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", ea.Name, err))
			continue
		}
		result.Restored++
	}
	return result, nil
}

// ImportResult summarises what happened during an import.
type ImportResult struct {
	Restored int
	Skipped  int
	Errors   []string
}

// Import reads a JSON export payload and restores apps into the store.
// Apps with the same ID are skipped.
func Import(r io.Reader, s *store.AppStore) (*ImportResult, error) {
	ef, err := ParseExport(r)
	if err != nil {
		return nil, err
	}
	return ImportParsed(ef, s)
}

// exportedToApp converts an ExportedApp to a store.App ready for creation.
func exportedToApp(ea ExportedApp) *store.App {
	app := &store.App{
		ID:              ea.ID,
		Name:            ea.Name,
		ImageRef:        ea.ImageRef,
		Entrypoint:      ea.Entrypoint,
		Command:         ea.Command,
		WorkDir:         ea.WorkDir,
		// RootfsPath intentionally left empty — image must be re-pulled on new host.
		EnvVars:         ea.EnvVars,
		Status:          store.StatusStopped,
		AutoStart:       ea.AutoStart,
		ExposedPort:     ea.ExposedPort,
		HealthEndpoint:  ea.HealthEndpoint,
		PruneAfterStart: ea.PruneAfterStart,
	}
	if app.Entrypoint == nil {
		app.Entrypoint = []string{}
	}
	if app.Command == nil {
		app.Command = []string{}
	}
	return app
}
