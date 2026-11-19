package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// memStore is a TokenStore in memory. Its RotateToken keeps the pair when
// exchange fails, like the SQLite one.
type memStore struct {
	mu  sync.Mutex
	tok *Token
}

func (m *memStore) Token(context.Context) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tok == nil {
		return Token{}, fmt.Errorf("%w: none stored", ErrLoginRequired)
	}
	return *m.tok, nil
}

func (m *memStore) SaveToken(_ context.Context, t Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tok = &t
	return nil
}

func (m *memStore) RotateToken(_ context.Context, exchange func(Token) (Token, error)) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, err := exchange(*m.tok)
	if err != nil {
		return Token{}, err
	}
	m.tok = &next
	return next, nil
}

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func newTestClient(t *testing.T, h http.Handler, store *memStore) (*Client, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(Config{ClientID: "cid", APIURL: srv.URL, AuthURL: srv.URL, Version: "test", Store: store})
	c.now = func() time.Time { return now }
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return c, &slept
}

func valid() *memStore {
	return &memStore{tok: &Token{Access: "a1", Refresh: "r1", ExpiresAt: now.Add(7 * 24 * time.Hour)}}
}

func expired() *memStore {
	return &memStore{tok: &Token{Access: "a1", Refresh: "r1", ExpiresAt: now.Add(-time.Hour)}}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func tokenBody(access, refresh string) map[string]any {
	return map[string]any{
		"access_token": access, "refresh_token": refresh, "token_type": "bearer",
		"expires_in": 86400, "scope": "public", "created_at": now.Unix(),
	}
}

func TestEveryRequestSendsTraktHeaders(t *testing.T) {
	var seen []http.Header
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		writeJSON(w, tokenBody("a2", "r2"))
	})
	mux.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		writeJSON(w, map[string]any{"all": now})
	})
	c, _ := newTestClient(t, mux, expired())
	if _, err := c.LastActivities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("got %d requests, want a refresh and a GET", len(seen))
	}
	for _, h := range seen {
		for name, want := range map[string]string{
			"trakt-api-key":     "cid",
			"trakt-api-version": "2",
			"Content-Type":      "application/json",
			"User-Agent":        "trakt-micropub/test",
		} {
			if got := h.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
	}
	if got := seen[1].Get("Authorization"); got != "Bearer a2" {
		t.Errorf("Authorization = %q, want the refreshed token", got)
	}
}

func TestRefreshStoresThePairBeforeUsingIt(t *testing.T) {
	store := expired()
	var refreshBody map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&refreshBody)
		writeJSON(w, tokenBody("a2", "r2"))
	})
	mux.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
		stored, _ := store.Token(r.Context())
		if stored.Refresh != "r2" {
			t.Errorf("the GET arrived while the store held refresh token %q", stored.Refresh)
		}
		writeJSON(w, map[string]any{"all": now})
	})
	c, _ := newTestClient(t, mux, store)
	if _, err := c.LastActivities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshBody["refresh_token"] != "r1" || refreshBody["grant_type"] != "refresh_token" || refreshBody["redirect_uri"] != RedirectURI {
		t.Errorf("refresh body = %v", refreshBody)
	}
	got, _ := store.Token(context.Background())
	// The lifetime comes from expires_in: 86400 seconds after created_at.
	want := Token{Access: "a2", Refresh: "r2", ExpiresAt: now.Add(24 * time.Hour)}
	if got != want {
		t.Errorf("stored %+v, want %+v", got, want)
	}
}

func TestFailedRefreshKeepsTheOldPairAndAsksForLogin(t *testing.T) {
	for name, answer := range map[string]http.HandlerFunc{
		"invalid_grant": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": "session not found"})
		},
		"captcha page with 200": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!DOCTYPE html><title>Just a moment...</title>")
		},
		"200 without tokens": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]string{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := expired()
			before, _ := store.Token(context.Background())
			mux := http.NewServeMux()
			mux.HandleFunc("POST /oauth/token", answer)
			mux.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
				t.Error("the GET went out after a failed refresh")
			})
			c, _ := newTestClient(t, mux, store)
			_, err := c.LastActivities(context.Background())
			if !errors.Is(err, ErrLoginRequired) {
				t.Fatalf("err = %v, want ErrLoginRequired", err)
			}
			if after, _ := store.Token(context.Background()); after != before {
				t.Errorf("the pair changed to %+v", after)
			}
		})
	}
}

func TestServerErrorOnRefreshIsNotALostLogin(t *testing.T) {
	store := expired()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	c, _ := newTestClient(t, mux, store)
	_, err := c.LastActivities(context.Background())
	if err == nil || errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v, want a plain error to retry next pass", err)
	}
	if got, _ := store.Token(context.Background()); got.Refresh != "r1" {
		t.Errorf("refresh token = %q, want r1 kept", got.Refresh)
	}
}

func TestUnauthorizedRefreshesOnceAndRetries(t *testing.T) {
	store := valid()
	refreshes := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		writeJSON(w, tokenBody("a2", "r2"))
	})
	mux.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer a2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"all": now})
	})
	c, _ := newTestClient(t, mux, store)
	if _, err := c.LastActivities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Errorf("refreshed %d times, want 1", refreshes)
	}
}

func TestTooManyRequestsHonoursRetryAfter(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, map[string]any{"all": now})
	})
	c, slept := newTestClient(t, mux, valid())
	if _, err := c.LastActivities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(*slept) != 1 || (*slept)[0] != 7*time.Second {
		t.Errorf("calls = %d, slept = %v, want a 7s wait and one retry", calls, *slept)
	}
}

func TestTooManyRequestsGivesUpEventually(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sync/last_activities", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, slept := newTestClient(t, mux, valid())
	_, err := c.LastActivities(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want a 429 StatusError", err)
	}
	if len(*slept) != maxRetries {
		t.Errorf("waited %d times, want %d", len(*slept), maxRetries)
	}
}

func TestRetryAfter(t *testing.T) {
	for h, want := range map[string]time.Duration{
		"":     -1,
		"soon": -1,
		"0":    0,
		"30":   30 * time.Second,
		now.Add(time.Minute).Format(http.TimeFormat): time.Minute,
	} {
		if got := retryAfter(h, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestLoginPollsUntilApproved(t *testing.T) {
	store := &memStore{}
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"device_code": "dev", "user_code": "ABCD1234",
			"verification_url": "https://trakt.tv/activate", "expires_in": 600, "interval": 5,
		})
	})
	mux.HandleFunc("POST /oauth/device/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["code"] != "dev" || body["client_id"] != "cid" {
			t.Errorf("poll body = %v", body)
		}
		polls++
		switch polls {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			writeJSON(w, tokenBody("a1", "r1"))
		}
	})
	c, slept := newTestClient(t, mux, store)
	var shown DeviceCode
	if err := c.Login(context.Background(), func(dc DeviceCode) { shown = dc }); err != nil {
		t.Fatal(err)
	}
	if shown.UserCode != "ABCD1234" {
		t.Errorf("showed %+v", shown)
	}
	if got, _ := store.Token(context.Background()); got.Access != "a1" || got.Refresh != "r1" {
		t.Errorf("stored %+v", got)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}
	if fmt.Sprint(*slept) != fmt.Sprint(want) {
		t.Errorf("waited %v, want %v (slowed down after the 429)", *slept, want)
	}
}

func TestLoginDenied(t *testing.T) {
	store := &memStore{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"device_code": "dev", "user_code": "X", "verification_url": "u", "expires_in": 600, "interval": 1})
	})
	mux.HandleFunc("POST /oauth/device/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	c, _ := newTestClient(t, mux, store)
	if err := c.Login(context.Background(), func(DeviceCode) {}); err == nil {
		t.Fatal("a denied login succeeded")
	}
	if store.tok != nil {
		t.Errorf("stored %+v after a denied login", store.tok)
	}
}
