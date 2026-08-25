package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	_ "modernc.org/sqlite"
)

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*sql.DB, error) {
	// modernc.org/sqlite only honors the _pragma=NAME(VALUE) query parameter
	// form — mattn-style _foreign_keys/_journal_mode parameters are silently
	// ignored, which previously left foreign_keys OFF (breaking ON DELETE
	// CASCADE) and never enabled WAL.
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite supports only one writer at a time; cap to 1 connection.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	log.Printf("database opened at %s", path)
	return db, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS apps (
			id                TEXT PRIMARY KEY,
			name              TEXT NOT NULL,
			image_ref         TEXT NOT NULL,
			entrypoint        TEXT NOT NULL DEFAULT '[]',
			command           TEXT NOT NULL DEFAULT '[]',
			work_dir          TEXT NOT NULL DEFAULT '',
			rootfs_path       TEXT NOT NULL DEFAULT '',
			status            TEXT NOT NULL DEFAULT 'stopped',
			auto_start        INTEGER NOT NULL DEFAULT 0,
			exposed_port      INTEGER NOT NULL DEFAULT 0,
			health_endpoint   TEXT NOT NULL DEFAULT '',
			prune_after_start INTEGER NOT NULL DEFAULT 0,
			created_at        TEXT NOT NULL,
			updated_at        TEXT NOT NULL,
			last_started      TEXT
		);

		CREATE TABLE IF NOT EXISTS env_vars (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			app_id  TEXT NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
			key     TEXT NOT NULL,
			value   TEXT NOT NULL
		);
	`)
	if err != nil {
		return err
	}

	// Idempotent additions for databases that predate these columns.
	// Only "duplicate column" errors are tolerated — anything else (I/O errors,
	// corruption) must surface instead of being silently swallowed.
	for _, stmt := range []string{
		`ALTER TABLE apps ADD COLUMN prune_after_start INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE apps ADD COLUMN rootfs_path TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migration %q: %w", stmt, err)
		}
	}
	return nil
}
