package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/niklasfyi/trakt-micropub/internal/state"
	"github.com/niklasfyi/trakt-micropub/internal/trakt"
)

// fakeTrakt answers on both the API and the auth host. Its handlers are set
// per test.
type fakeTrakt struct {
	*http.ServeMux
	url string
}

func newFakeTrakt(t *testing.T) *fakeTrakt {
	f := &fakeTrakt{ServeMux: http.NewServeMux()}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func tokenBody(access, refresh string) map[string]any {
	return map[string]any{
		"access_token": access, "refresh_token": refresh, "token_type": "bearer",
		"expires_in": 7 * 24 * 3600, "scope": "public", "created_at": time.Now().Unix(),
	}
}

func testEnv(f *fakeTrakt, statePath string) func(string) string {
	m := map[string]string{
		"TRAKT_CLIENT_ID":   "cid",
		"TRAKT_API_URL":     f.url,
		"TRAKT_AUTH_URL":    f.url,
		"MICROPUB_ENDPOINT": "https://example.com/micropub",
		"MICROPUB_TOKEN":    "mp",
		"SITE_URL":          "https://example.com/",
		"TMDB_API_KEY":      "tmdb",
		"STATE_PATH":        statePath,
	}
	return func(k string) string { return m[k] }
}

// storedRefresh reads the refresh token straight from the state file, as
// another reader would see it, without taking the lock.
func storedRefresh(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var r string
	if err := db.QueryRow(`SELECT refresh_token FROM trakt_token`).Scan(&r); err != nil {
		t.Fatalf("reading the stored token: %v", err)
	}
	return r
}

func seedToken(t *testing.T, path string, tok trakt.Token) {
	t.Helper()
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SaveToken(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
}

func lastActivities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"all": "2026-11-19T10:00:00.000Z"})
}

func TestLoginStoresTokens(t *testing.T) {
	f := newFakeTrakt(t)
	f.HandleFunc("POST /oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"device_code": "dev", "user_code": "ABCD1234",
			"verification_url": "https://trakt.tv/activate", "expires_in": 600, "interval": 0,
		})
	})
	f.HandleFunc("POST /oauth/device/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, tokenBody("a1", "r1"))
	})
	path := filepath.Join(t.TempDir(), "state.db")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"login"}, testEnv(f, path), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ABCD1234") || !strings.Contains(stdout.String(), "https://trakt.tv/activate") {
		t.Errorf("stdout doesn't show the code and URL: %s", stdout.String())
	}
	if got := storedRefresh(t, path); got != "r1" {
		t.Errorf("stored refresh token %q, want r1", got)
	}
}

func TestSecondRunExitsWhileOneRuns(t *testing.T) {
	f := newFakeTrakt(t)
	polled := make(chan struct{})
	var once sync.Once
	f.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
		lastActivities(w, r)
		once.Do(func() { close(polled) })
	})
	path := filepath.Join(t.TempDir(), "state.db")
	seedToken(t, path, trakt.Token{Access: "a1", Refresh: "r1", ExpiresAt: time.Now().Add(7 * 24 * time.Hour)})
	env := testEnv(f, path)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{"run"}, env, &out, &errOut)
	}()
	select {
	case <-polled:
	case <-time.After(10 * time.Second):
		t.Fatal("the first run never polled")
	}

	for _, cmd := range []string{"run", "once", "login"} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), []string{cmd}, env, &stdout, &stderr)
		if code != 1 {
			t.Errorf("%s: exit %d, want 1", cmd, code)
		}
		if msg := stderr.String(); !strings.Contains(msg, "in use by another trakt-micropub process") || !strings.Contains(msg, "Stop the other one first") {
			t.Errorf("%s: unclear message: %s", cmd, msg)
		}
	}

	cancel()
	if code := <-done; code != 0 {
		t.Errorf("the first run exited %d after a stop", code)
	}
}

func TestRefreshedPairIsStoredBeforeTheNextRequest(t *testing.T) {
	f := newFakeTrakt(t)
	path := filepath.Join(t.TempDir(), "state.db")
	f.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, tokenBody("a2", "r2"))
	})
	f.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer a2" {
			t.Errorf("Authorization = %q, want the new token", got)
		}
		if got := storedRefresh(t, path); got != "r2" {
			t.Errorf("the request came in while the file held refresh token %q", got)
		}
		lastActivities(w, r)
	})
	seedToken(t, path, trakt.Token{Access: "a1", Refresh: "r1", ExpiresAt: time.Now().Add(-time.Hour)})

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"once"}, testEnv(f, path), &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestFailedRefreshStopsWithLoginHint(t *testing.T) {
	for _, cmd := range []string{"once", "run"} {
		t.Run(cmd, func(t *testing.T) {
			f := newFakeTrakt(t)
			f.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"invalid_grant","error_description":"session not found"}`))
			})
			f.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
				t.Error("a request went out after the refresh failed")
			})
			path := filepath.Join(t.TempDir(), "state.db")
			seedToken(t, path, trakt.Token{Access: "a1", Refresh: "r1", ExpiresAt: time.Now().Add(-time.Hour)})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			if code := run(ctx, []string{cmd}, testEnv(f, path), &stdout, &stderr); code != 1 {
				t.Fatalf("exit %d, want 1: %s", code, stderr.String())
			}
			if ctx.Err() != nil {
				t.Fatal("run kept going after the refresh failed")
			}
			if !strings.Contains(stderr.String(), "trakt-micropub login") {
				t.Errorf("no login hint: %s", stderr.String())
			}
			if got := storedRefresh(t, path); got != "r1" {
				t.Errorf("stored refresh token %q, want r1 untouched", got)
			}
		})
	}
}

func TestOnceWithoutLoginAsksForLogin(t *testing.T) {
	f := newFakeTrakt(t)
	path := filepath.Join(t.TempDir(), "state.db")
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"once"}, testEnv(f, path), &stdout, &stderr); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "trakt-micropub login") {
		t.Errorf("no login hint: %s", stderr.String())
	}
}
