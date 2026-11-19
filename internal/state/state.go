// Package state keeps the client's state in one SQLite file, which one
// process at a time may hold.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrLocked means another process holds the state file.
var ErrLocked = errors.New("state: the state file is in use by another trakt-micropub process")

// Store is the open state file.
type Store struct {
	db   *sql.DB
	lock *lockFile
}

// Open takes the lock on the state file at path, then opens it, creating it
// when it doesn't exist, and brings its schema up to date. When another
// process holds the lock, it answers an error wrapping ErrLocked.
func Open(path string) (*Store, error) {
	lock, err := acquireLock(path + ".lock")
	if err != nil {
		return nil, err
	}
	// Transactions take the write lock when they begin.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		lock.release()
		return nil, fmt.Errorf("state: opening %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		lock.release()
		return nil, err
	}
	return &Store{db: db, lock: lock}, nil
}

// Close closes the database and releases the lock.
func (s *Store) Close() error {
	err := s.db.Close()
	s.lock.release()
	return err
}

func (s *Store) write(ctx context.Context, do func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := do(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339, s) }
