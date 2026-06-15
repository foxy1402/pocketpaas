package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type AppStore struct {
	db *sql.DB
}

func NewAppStore(db *sql.DB) *AppStore {
	return &AppStore{db: db}
}

func (s *AppStore) Create(app *App) error {
	ep, err := json.Marshal(app.Entrypoint)
	if err != nil {
		return fmt.Errorf("marshal entrypoint: %w", err)
	}
	cmd, err := json.Marshal(app.Command)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	now := time.Now().UTC()
	app.CreatedAt = now
	app.UpdatedAt = now

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO apps (id, name, image_ref, entrypoint, command, work_dir, rootfs_path,
		                  status, auto_start, exposed_port, health_endpoint, prune_after_start,
		                  created_at, updated_at, last_started)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		app.ID, app.Name, app.ImageRef, string(ep), string(cmd),
		app.WorkDir, app.RootfsPath, string(app.Status),
		boolToInt(app.AutoStart), app.ExposedPort, app.HealthEndpoint,
		boolToInt(app.PruneAfterStart),
		now.Format(time.RFC3339), now.Format(time.RFC3339), nil,
	)
	if err != nil {
		return fmt.Errorf("insert app: %w", err)
	}

	if err := insertEnvVars(tx, app.ID, app.EnvVars); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *AppStore) Update(app *App) error {
	ep, err := json.Marshal(app.Entrypoint)
	if err != nil {
		return fmt.Errorf("marshal entrypoint: %w", err)
	}
	cmd, err := json.Marshal(app.Command)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}
	app.UpdatedAt = time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		UPDATE apps SET name=?, image_ref=?, entrypoint=?, command=?,
		                auto_start=?, exposed_port=?, health_endpoint=?,
		                prune_after_start=?, updated_at=?
		WHERE id=?`,
		app.Name, app.ImageRef, string(ep), string(cmd),
		boolToInt(app.AutoStart), app.ExposedPort, app.HealthEndpoint,
		boolToInt(app.PruneAfterStart),
		app.UpdatedAt.Format(time.RFC3339), app.ID,
	)
	if err != nil {
		return fmt.Errorf("update app: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM env_vars WHERE app_id=?`, app.ID); err != nil {
		return err
	}
	if err := insertEnvVars(tx, app.ID, app.EnvVars); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *AppStore) UpdateStatus(id string, status AppStatus) error {
	_, err := s.db.Exec(`UPDATE apps SET status=?, updated_at=? WHERE id=?`,
		string(status), time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// UpdateRootfsPath stores the host path to the extracted image filesystem.
func (s *AppStore) UpdateRootfsPath(id, rootfsPath string) error {
	_, err := s.db.Exec(`UPDATE apps SET rootfs_path=?, updated_at=? WHERE id=?`,
		rootfsPath, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// UpdateWorkDir stores the container-internal working directory (image WORKDIR).
func (s *AppStore) UpdateWorkDir(id, workDir string) error {
	_, err := s.db.Exec(`UPDATE apps SET work_dir=?, updated_at=? WHERE id=?`,
		workDir, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *AppStore) UpdateEntrypointCmd(id string, entrypoint, command []string) error {
	ep, err := json.Marshal(entrypoint)
	if err != nil {
		return err
	}
	cmd, err := json.Marshal(command)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE apps SET entrypoint=?, command=?, updated_at=? WHERE id=?`,
		string(ep), string(cmd), time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (s *AppStore) SetLastStarted(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`UPDATE apps SET last_started=?, updated_at=? WHERE id=?`,
		now, now, id)
	return err
}

func (s *AppStore) Get(id string) (*App, error) {
	row := s.db.QueryRow(`
		SELECT id, name, image_ref, entrypoint, command, work_dir, rootfs_path,
		       status, auto_start, exposed_port, health_endpoint, prune_after_start,
		       created_at, updated_at, last_started
		FROM apps WHERE id=?`, id)
	app, err := scanApp(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	app.EnvVars, err = s.getEnvVars(id)
	return app, err
}

func (s *AppStore) List() ([]*App, error) {
	rows, err := s.db.Query(`
		SELECT id, name, image_ref, entrypoint, command, work_dir, rootfs_path,
		       status, auto_start, exposed_port, health_endpoint, prune_after_start,
		       created_at, updated_at, last_started
		FROM apps ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*App
	for rows.Next() {
		app, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		app.EnvVars, err = s.getEnvVars(app.ID)
		if err != nil {
			return nil, err
		}
		apps = append(apps, app)
	}
	return apps, rows.Err()
}

func (s *AppStore) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM apps WHERE id=?`, id)
	return err
}

func (s *AppStore) ResetRunningToStopped() error {
	_, err := s.db.Exec(`UPDATE apps SET status='stopped' WHERE status='running' OR status='pulling'`)
	return err
}

func (s *AppStore) getEnvVars(appID string) ([]EnvVar, error) {
	rows, err := s.db.Query(`SELECT key, value FROM env_vars WHERE app_id=? ORDER BY id`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vars []EnvVar
	for rows.Next() {
		var ev EnvVar
		if err := rows.Scan(&ev.Key, &ev.Value); err != nil {
			return nil, err
		}
		vars = append(vars, ev)
	}
	return vars, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanApp(s scanner) (*App, error) {
	var app App
	var epJSON, cmdJSON string
	var statusStr string
	var autoStart, pruneAfterStart int
	var createdAt, updatedAt string
	var lastStarted sql.NullString

	err := s.Scan(
		&app.ID, &app.Name, &app.ImageRef, &epJSON, &cmdJSON,
		&app.WorkDir, &app.RootfsPath,
		&statusStr, &autoStart, &app.ExposedPort, &app.HealthEndpoint, &pruneAfterStart,
		&createdAt, &updatedAt, &lastStarted,
	)
	if err != nil {
		return nil, err
	}

	app.Status = AppStatus(statusStr)
	app.AutoStart = autoStart == 1
	app.PruneAfterStart = pruneAfterStart == 1
	if err := json.Unmarshal([]byte(epJSON), &app.Entrypoint); err != nil {
		app.Entrypoint = nil
	}
	if err := json.Unmarshal([]byte(cmdJSON), &app.Command); err != nil {
		app.Command = nil
	}
	app.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	app.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if lastStarted.Valid {
		t, _ := time.Parse(time.RFC3339, lastStarted.String)
		app.LastStarted = &t
	}
	return &app, nil
}

func insertEnvVars(tx *sql.Tx, appID string, vars []EnvVar) error {
	for _, ev := range vars {
		if ev.Key == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO env_vars (app_id, key, value) VALUES (?, ?, ?)`,
			appID, ev.Key, ev.Value); err != nil {
			return fmt.Errorf("insert env var: %w", err)
		}
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
