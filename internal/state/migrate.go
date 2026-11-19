package state

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"time"
)

// Migrations are named after the issue that adds them, 0001_trakt_token.sql
// for #1, and run in the order of their names. Every migration the database
// hasn't had runs, not only those after the last one it had.
//
//go:embed migrations/*.sql
var migrations embed.FS

// migrate runs the migrations the database hasn't had, each in a transaction
// together with the record that it ran.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		name    TEXT PRIMARY KEY,
		applied TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("state: creating the migrations table: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	for _, file := range names {
		name := path.Base(file)
		var ran int
		if err := db.QueryRow(`SELECT count(*) FROM migrations WHERE name = ?`, name).Scan(&ran); err != nil {
			return fmt.Errorf("state: checking migration %s: %w", name, err)
		}
		if ran > 0 {
			continue
		}
		statements, err := migrations.ReadFile(file)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(statements)); err != nil {
			tx.Rollback()
			return fmt.Errorf("state: migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO migrations (name, applied) VALUES (?, ?)`, name, formatTime(time.Now())); err != nil {
			tx.Rollback()
			return fmt.Errorf("state: migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("state: migration %s: %w", name, err)
		}
	}
	return nil
}
