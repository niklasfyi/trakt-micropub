package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTrakt keeps one history and edits the edit test's play (id changing or
// not) after it has been read a few times.
type fakeTrakt struct {
	mu        sync.Mutex
	next      int64
	plays     []map[string]any
	reads     int
	newIDEdit bool
	removed   []int64
}

func (f *fakeTrakt) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/oauth/device/code":
		json.NewEncoder(w).Encode(map[string]any{"device_code": "d", "user_code": "U", "verification_url": "x", "expires_in": 60, "interval": 0})
	case p == "/oauth/device/token":
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok"})
	case p == "/oauth/revoke":
	case strings.HasPrefix(p, "/movies/"):
		id := map[string]int64{"/movies/m": 10, "/movies/e": 20}[p]
		json.NewEncoder(w).Encode(map[string]any{"title": p, "year": 1, "ids": map[string]any{"trakt": id}})
	case strings.HasPrefix(p, "/shows/"):
		id := int64(30)
		if strings.HasSuffix(p, "/2") {
			id = 31
		}
		json.NewEncoder(w).Encode(map[string]any{"title": "ep", "ids": map[string]any{"trakt": id}})
	case p == "/sync/history" && r.Method == http.MethodPost:
		var body map[string][]struct {
			IDs       struct{ Trakt int64 } `json:"ids"`
			WatchedAt string                `json:"watched_at"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for kind, items := range body {
			for _, it := range items {
				if kind == "movies" && it.WatchedAt == "released" {
					json.NewEncoder(w).Encode(map[string]any{"added": map[string]int{"movies": 0}})
					return
				}
				wa := it.WatchedAt
				if wa == "unknown" {
					wa = "1970-01-01T00:00:00.000Z"
				}
				if wa == "released" {
					wa = "1975-09-19T21:00:00.000Z"
				}
				f.next++
				f.plays = append(f.plays, map[string]any{"id": f.next, "watched_at": wa, "action": "watch", "type": strings.TrimSuffix(kind, "s"), "item": it.IDs.Trakt})
			}
			json.NewEncoder(w).Encode(map[string]any{"added": map[string]int{kind: len(items)}})
		}
	case p == "/sync/history/remove":
		var body struct{ IDs []int64 }
		json.NewDecoder(r.Body).Decode(&body)
		f.removed = body.IDs
		json.NewEncoder(w).Encode(map[string]any{"deleted": map[string]int{"movies": len(body.IDs)}})
	case p == "/sync/history":
		json.NewEncoder(w).Encode(f.plays)
	case strings.HasPrefix(p, "/sync/history/movies/20"):
		f.reads++
		if f.reads == 3 {
			for i, pl := range f.plays {
				if pl["item"] == int64(20) {
					if f.newIDEdit {
						f.next++
						pl = map[string]any{"id": f.next, "watched_at": "2001-01-02T12:00:00.000Z", "item": int64(20)}
						f.plays[i] = pl
					} else {
						pl["watched_at"] = "2001-01-02T12:00:00.000Z"
					}
				}
			}
		}
		f.item(w, 20)
	case strings.HasPrefix(p, "/sync/history/"):
		var id int64
		json.Unmarshal([]byte(p[strings.LastIndex(p, "/")+1:]), &id)
		f.item(w, id)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeTrakt) item(w http.ResponseWriter, id int64) {
	out := []map[string]any{}
	for _, pl := range f.plays {
		if pl["item"] == id {
			out = append(out, pl)
		}
	}
	json.NewEncoder(w).Encode(out)
}

func TestProbe(t *testing.T) {
	for _, newID := range []bool{false, true} {
		f := &fakeTrakt{newIDEdit: newID}
		s := httptest.NewServer(f)
		apiURL, authURL, clientID, token, added = s.URL, s.URL, "c", "", nil
		report.Reset()
		if err := run(context.Background(), "m", "e", "s", time.Minute); err != nil {
			t.Fatal(err)
		}
		cleanup(context.Background())
		t.Log(report.String())
		want := "**The id stays the same.**"
		if newID {
			want = "**The id changes.**"
		}
		if !strings.Contains(report.String(), want) {
			t.Errorf("newID=%v: report lacks %q", newID, want)
		}
		if len(f.removed) != int(f.next) {
			t.Errorf("newID=%v: removed %v, made %d plays", newID, f.removed, f.next)
		}
		s.Close()
	}
}
