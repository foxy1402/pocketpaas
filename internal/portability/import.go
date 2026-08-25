package portability

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"apphive/internal/store"
)

// MaxExportBytes caps the size of an import payload. Exports contain app
// configs and env vars only — a legitimate file is kilobytes, so 32 MiB is
// generous while preventing memory-exhaustion via ReadAll on a huge upload.
const MaxExportBytes = 32 << 20

// ParseExport reads and validates a JSON export from r without importing to the store.
func ParseExport(r io.Reader) (*ExportFile, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxExportBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(data) > MaxExportBytes {
		return nil, fmt.Errorf("export file exceeds the %d MiB size limit", MaxExportBytes>>20)
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

// validAppID reports whether id is safe to use: it ends up joined into
// filesystem paths (deleteApp's RemoveAll, pull extract destinations), so it
// must never contain separators or traversal segments.
func validAppID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	// reject ".", "..", and anything separator-like (defense in depth)
	return !strings.Contains(id, "..") && id != "."
}

// ImportParsed restores apps from an already-parsed ExportFile. Apps with
// existing IDs are skipped. This is useful when you need to parse first
// (e.g. for validation) then import.
func ImportParsed(ef *ExportFile, s *store.AppStore) (*ImportResult, error) {
	result := &ImportResult{}
	for _, ea := range ef.Apps {
		if !validAppID(ea.ID) {
			result.Errors = append(result.Errors,
				fmt.Sprintf("%s: invalid app id %q (ids are used in filesystem paths)", ea.Name, ea.ID))
			continue
		}
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
		ID:         ea.ID,
		Name:       ea.Name,
		ImageRef:   ea.ImageRef,
		Entrypoint: ea.Entrypoint,
		Command:    ea.Command,
		WorkDir:    ea.WorkDir,
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
