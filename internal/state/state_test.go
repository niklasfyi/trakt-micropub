package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/niklasfyi/trakt-micropub/internal/trakt"
)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestOnlyOneProcessHoldsTheStateFile(t *testing.T) {
	s, path := openTemp(t)
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open: err = %v, want ErrLocked", err)
	}
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	again.Close()
}

func TestTokenWithoutLoginAsksForLogin(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.Token(context.Background()); !errors.Is(err, trakt.ErrLoginRequired) {
		t.Fatalf("err = %v, want ErrLoginRequired", err)
	}
}

func TestTokenSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t)
	want := trakt.Token{Access: "a", Refresh: "r", ExpiresAt: time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)}
	if err := s.SaveToken(ctx, want); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestRotateToken(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t)
	old := trakt.Token{Access: "a1", Refresh: "r1", ExpiresAt: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	if err := s.SaveToken(ctx, old); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("refused")
	_, err := s.RotateToken(ctx, func(got trakt.Token) (trakt.Token, error) {
		if got != old {
			t.Errorf("exchange got %+v, want the stored pair", got)
		}
		return trakt.Token{}, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the exchange's error", err)
	}
	if got, _ := s.Token(ctx); got != old {
		t.Errorf("after a failed exchange the pair is %+v, want it untouched", got)
	}

	next := trakt.Token{Access: "a2", Refresh: "r2", ExpiresAt: old.ExpiresAt.Add(24 * time.Hour)}
	if _, err := s.RotateToken(ctx, func(trakt.Token) (trakt.Token, error) { return next, nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Token(ctx); got != next {
		t.Errorf("after a rotation the pair is %+v, want %+v", got, next)
	}
}
