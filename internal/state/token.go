package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/niklasfyi/trakt-micropub/internal/trakt"
)

var _ trakt.TokenStore = (*Store)(nil)

// Token returns the stored Trakt token pair.
func (s *Store) Token(ctx context.Context) (trakt.Token, error) {
	return readToken(ctx, s.db)
}

// SaveToken replaces the stored Trakt token pair.
func (s *Store) SaveToken(ctx context.Context, t trakt.Token) error {
	return s.write(ctx, func(tx *sql.Tx) error { return writeToken(ctx, tx, t) })
}

// RotateToken hands the stored pair to exchange and stores what it returns,
// all in one transaction, so a pair Trakt hands out is on disk before it is
// used. When exchange fails, the transaction rolls back and the stored pair
// stays.
func (s *Store) RotateToken(ctx context.Context, exchange func(old trakt.Token) (trakt.Token, error)) (trakt.Token, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return trakt.Token{}, err
	}
	old, err := readToken(ctx, tx)
	if err != nil {
		tx.Rollback()
		return trakt.Token{}, err
	}
	next, err := exchange(old)
	if err != nil {
		tx.Rollback()
		return trakt.Token{}, err
	}
	if err := writeToken(ctx, tx, next); err != nil {
		tx.Rollback()
		return trakt.Token{}, spent(err)
	}
	if err := tx.Commit(); err != nil {
		return trakt.Token{}, spent(err)
	}
	return next, nil
}

// spent wraps a failure to store a pair Trakt already handed out. The old
// refresh token is used up by then, so only a new login helps.
func spent(err error) error {
	return fmt.Errorf("%w: storing the refreshed token pair failed after Trakt rotated it: %w", trakt.ErrLoginRequired, err)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readToken(ctx context.Context, q querier) (trakt.Token, error) {
	var t trakt.Token
	var expires string
	err := q.QueryRowContext(ctx, `SELECT access_token, refresh_token, expires_at FROM trakt_token WHERE id = 1`).
		Scan(&t.Access, &t.Refresh, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return trakt.Token{}, fmt.Errorf("%w: no Trakt login is stored", trakt.ErrLoginRequired)
	}
	if err != nil {
		return trakt.Token{}, fmt.Errorf("state: reading the Trakt token: %w", err)
	}
	if t.ExpiresAt, err = parseTime(expires); err != nil {
		return trakt.Token{}, fmt.Errorf("state: reading the Trakt token: %w", err)
	}
	return t, nil
}

func writeToken(ctx context.Context, tx *sql.Tx, t trakt.Token) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO trakt_token (id, access_token, refresh_token, expires_at, updated_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			access_token = excluded.access_token,
			refresh_token = excluded.refresh_token,
			expires_at = excluded.expires_at,
			updated_at = excluded.updated_at`,
		t.Access, t.Refresh, formatTime(t.ExpiresAt), formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("state: storing the Trakt token: %w", err)
	}
	return nil
}
