package store

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite"
)

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_foreign_keys=on&_journal_mode=WAL")
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
	db.Exec(`ALTER TABLE apps ADD COLUMN prune_after_start INTEGER NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE apps ADD COLUMN rootfs_path TEXT NOT NULL DEFAULT ''`)
	return nil
}
