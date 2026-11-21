// Command probe answers niklasfyi/trakt-micropub#2 against a real Trakt
// account:
//
//  1. What watched_at does GET /sync/history give a play added with
//     watched_at "unknown" (a movie and an episode) or "released" (an
//     episode, and a movie to see whether Trakt takes it)? Do those plays
//     show up in a 14-day start_at/end_at window?
//  2. Does a play's history id stay the same when its date is changed in
//     the Trakt web app?
//
// It signs in with a device code and keeps the token in memory only, so the
// login of trakt-micropub's state file is untouched; the token is revoked at
// the end. Every play it adds is removed again by its history id, also on
// Ctrl-C. The answers and the raw JSON go to a Markdown file.
//
//	TRAKT_CLIENT_ID=… go run ./cmd/probe [-out results.md]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"
)

var (
	clientID     = os.Getenv("TRAKT_CLIENT_ID")
	clientSecret = os.Getenv("TRAKT_CLIENT_SECRET")
	apiURL       = envOr("TRAKT_API_URL", "https://api.trakt.tv")
	authURL      = envOr("TRAKT_AUTH_URL", "https://auth.trakt.tv")
	token        string
	httpc        = &http.Client{Timeout: 30 * time.Second}
	report       strings.Builder
	lastWrite    time.Time
)

// The plays this run added, by history id, so cleanup removes only those.
var added []int64

func main() {
	movieSlug := flag.String("movie", "plan-9-from-outer-space-1959", "Trakt slug of the movie for the unknown and released plays")
	editSlug := flag.String("edit-movie", "the-room-2003", "Trakt slug of the movie for the play you edit in the web app")
	showSlug := flag.String("show", "fawlty-towers", "Trakt slug of the show; S01E01 gets the unknown play, S01E02 the released one")
	out := flag.String("out", "trakt-probe-results.md", "where the answers go")
	wait := flag.Duration("wait", 20*time.Minute, "how long to wait for the edit in the web app")
	flag.Parse()
	if clientID == "" {
		fail(errors.New("set TRAKT_CLIENT_ID to the Trakt app's client id"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err := run(ctx, *movieSlug, *editSlug, *showSlug, *wait)
	// Clean up with a fresh context, so Ctrl-C still removes the plays.
	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cleanup(cctx)
	revoke(cctx)
	if werr := os.WriteFile(*out, []byte(report.String()), 0o644); werr != nil {
		fmt.Fprintln(os.Stderr, "writing the report:", werr)
	} else {
		fmt.Printf("\nThe answers are in %s.\n", *out)
	}
	if err != nil {
		fail(err)
	}
}

type ids struct {
	Trakt int64  `json:"trakt"`
	Slug  string `json:"slug,omitempty"`
}

type entry struct {
	ID        int64           `json:"id"`
	WatchedAt *string         `json:"watched_at"`
	Action    string          `json:"action"`
	Type      string          `json:"type"`
	Raw       json.RawMessage `json:"-"`
}

func run(ctx context.Context, movieSlug, editSlug, showSlug string, wait time.Duration) error {
	fmt.Fprintf(&report, "# Trakt probe for #2\n\nRun %s against %s.\n\n", time.Now().UTC().Format(time.RFC3339), apiURL)

	if err := login(ctx); err != nil {
		return err
	}

	movie, err := lookupMovie(ctx, movieSlug)
	if err != nil {
		return err
	}
	editMovie, err := lookupMovie(ctx, editSlug)
	if err != nil {
		return err
	}
	ep1, err := lookupEpisode(ctx, showSlug, 1, 1)
	if err != nil {
		return err
	}
	ep2, err := lookupEpisode(ctx, showSlug, 1, 2)
	if err != nil {
		return err
	}

	// Part 1: unknown and released plays.
	fmt.Println("\n== Part 1: unknown-date and release-date plays")
	fmt.Fprintf(&report, "## 1. Unknown-date and release-date plays\n\n")
	type probe struct {
		label, kind string
		id          int64
		watchedAt   string
	}
	probes := []probe{
		{"movie, watched_at \"unknown\" (" + movieSlug + ")", "movies", movie, "unknown"},
		{"episode, watched_at \"unknown\" (" + showSlug + " S01E01)", "episodes", ep1, "unknown"},
		{"episode, watched_at \"released\" (" + showSlug + " S01E02)", "episodes", ep2, "released"},
		{"movie, watched_at \"released\" (" + movieSlug + ")", "movies", movie, "released"},
	}
	fmt.Fprintf(&report, "| Play added | Trakt's add answer | History id | watched_at in `GET /sync/history` | In the 14-day window? |\n|---|---|---|---|---|\n")
	var raws []string
	for _, p := range probes {
		before, err := itemHistory(ctx, p.kind, p.id)
		if err != nil {
			return err
		}
		answer, err := addPlay(ctx, p.kind, p.id, p.watchedAt)
		if err != nil {
			fmt.Fprintf(&report, "| %s | refused: %s | | | |\n", p.label, oneLine(err.Error()))
			fmt.Printf("  %s: Trakt refused it: %v\n", p.label, err)
			continue
		}
		after, err := itemHistory(ctx, p.kind, p.id)
		if err != nil {
			return err
		}
		news := newEntries(idsOf(before), after)
		for _, e := range news {
			added = append(added, e.ID)
		}
		if len(news) == 0 {
			fmt.Fprintf(&report, "| %s | %s | none in the item's history | | |\n", p.label, oneLine(answer))
			fmt.Printf("  %s: added (%s), but no new entry in the item's history\n", p.label, answer)
			continue
		}
		window, err := windowIDs(ctx)
		if err != nil {
			return err
		}
		for _, e := range news {
			in := "no"
			if window[e.ID] {
				in = "yes"
			}
			fmt.Fprintf(&report, "| %s | %s | %d | %s | %s |\n", p.label, oneLine(answer), e.ID, watchedAt(e), in)
			fmt.Printf("  %s: history id %d, watched_at %s, in the 14-day window: %s\n", p.label, e.ID, watchedAt(e), in)
			raws = append(raws, fmt.Sprintf("%s:\n\n```json\n%s\n```\n", p.label, indent(e.Raw)))
		}
	}
	fmt.Fprintf(&report, "\nThe add answers count `added` and `not_found` from `POST /sync/history`. The 14-day window is `GET /sync/history?start_at=<now-14d>&end_at=<now>`.\n\n")
	fmt.Fprintf(&report, "<details><summary>The history entries as Trakt returned them</summary>\n\n%s\n</details>\n\n", strings.Join(raws, "\n"))

	// Part 2: does an edit in the web app keep the history id?
	fmt.Println("\n== Part 2: play id on edit")
	fmt.Fprintf(&report, "## 2. Play id on edit\n\n")
	const date = "2001-01-01T12:00:00.000Z"
	before, err := itemHistory(ctx, "movies", editMovie)
	if err != nil {
		return err
	}
	if _, err := addPlay(ctx, "movies", editMovie, date); err != nil {
		return err
	}
	after, err := itemHistory(ctx, "movies", editMovie)
	if err != nil {
		return err
	}
	news := newEntries(idsOf(before), after)
	if len(news) != 1 {
		return fmt.Errorf("expected one new play of %s, found %d", editSlug, len(news))
	}
	orig := news[0]
	added = append(added, orig.ID)
	known := idsOf(after)
	fmt.Printf("\nAdded a play of %s on 1 January 2001, history id %d.\n", editSlug, orig.ID)
	fmt.Printf("Now open https://trakt.tv/movies/%s in the Trakt web app, find that play in its history,\n", editSlug)
	fmt.Printf("and change its date to any other day (2 January 2001, say). Don't remove it.\n")
	fmt.Printf("This waits up to %s and checks every 10 seconds. Ctrl-C stops and cleans up.\n", wait)

	deadline := time.Now().Add(wait)
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintf(&report, "Stopped before the edit was seen.\n")
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
		now, err := itemHistory(ctx, "movies", editMovie)
		if err != nil {
			return err
		}
		var same *entry
		for i := range now {
			if now[i].ID == orig.ID {
				same = &now[i]
			}
		}
		fresh := newEntries(known, now)
		for _, e := range fresh {
			added = append(added, e.ID)
		}
		switch {
		case same != nil && watchedAt(*same) != watchedAt(orig):
			fmt.Printf("  The id stayed %d; watched_at went from %s to %s.\n", orig.ID, watchedAt(orig), watchedAt(*same))
			fmt.Fprintf(&report, "**The id stays the same.** History id %d kept its id when its date was changed in the web app: watched_at went from %s to %s.\n\n", orig.ID, watchedAt(orig), watchedAt(*same))
			fmt.Fprintf(&report, "```json\n%s\n```\n", indent(same.Raw))
			return nil
		case same == nil && len(fresh) > 0:
			fmt.Printf("  The id changed: %d is gone and %d is there, watched_at %s.\n", orig.ID, fresh[0].ID, watchedAt(fresh[0]))
			fmt.Fprintf(&report, "**The id changes.** History id %d (watched_at %s) was gone after the edit, and a new play took its place:\n\n", orig.ID, watchedAt(orig))
			for _, e := range fresh {
				fmt.Fprintf(&report, "```json\n%s\n```\n", indent(e.Raw))
			}
			return nil
		case same == nil:
			fmt.Printf("  Play %d is gone and no new play is there. Was it removed rather than edited? Still waiting.\n", orig.ID)
		case len(fresh) > 0:
			fmt.Printf("  Play %d is unchanged, but a new play %d appeared. Still waiting.\n", orig.ID, fresh[0].ID)
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(&report, "No edit was seen within %s.\n", wait)
			return errors.New("no edit seen in time")
		}
	}
}

// login runs the device flow and keeps the access token in memory.
func login(ctx context.Context) error {
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	status, body, err := do(ctx, http.MethodPost, authURL+"/oauth/device/code", map[string]string{"client_id": clientID}, false)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("asking for a device code: %d %s", status, body)
	}
	if err := json.Unmarshal(body, &dc); err != nil {
		return err
	}
	fmt.Printf("Open %s and enter the code %s\nWaiting for approval…\n", dc.VerificationURL, dc.UserCode)
	req := map[string]string{"code": dc.DeviceCode, "client_id": clientID}
	if clientSecret != "" {
		req["client_secret"] = clientSecret
	}
	interval := time.Duration(dc.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		status, body, err := do(ctx, http.MethodPost, authURL+"/oauth/device/token", req, false)
		if err != nil {
			return err
		}
		switch status {
		case http.StatusOK:
			var t struct {
				AccessToken string `json:"access_token"`
			}
			if err := json.Unmarshal(body, &t); err != nil {
				return err
			}
			token = t.AccessToken
			fmt.Println("Signed in. The token stays in memory and is revoked at the end.")
			return nil
		case http.StatusBadRequest:
		case http.StatusTooManyRequests:
			interval += 5 * time.Second
		default:
			return fmt.Errorf("device login: %d %s", status, body)
		}
	}
	return errors.New("the code expired before it was approved")
}

func revoke(ctx context.Context) {
	if token == "" {
		return
	}
	req := map[string]string{"token": token, "client_id": clientID}
	if clientSecret != "" {
		req["client_secret"] = clientSecret
	}
	if status, body, err := do(ctx, http.MethodPost, authURL+"/oauth/revoke", req, false); err != nil || status != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Revoking the token failed (%d %s %v). Revoke it at https://trakt.tv/oauth/authorized_applications.\n", status, body, err)
		return
	}
	fmt.Println("Revoked the token.")
}

func lookupMovie(ctx context.Context, slug string) (int64, error) {
	var m struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
		IDs   ids    `json:"ids"`
	}
	if err := get(ctx, "/movies/"+url.PathEscape(slug), nil, &m); err != nil {
		return 0, fmt.Errorf("looking up the movie %q (pick another with -movie or -edit-movie): %w", slug, err)
	}
	fmt.Printf("Movie: %s (%d), trakt id %d\n", m.Title, m.Year, m.IDs.Trakt)
	return m.IDs.Trakt, nil
}

func lookupEpisode(ctx context.Context, show string, season, number int) (int64, error) {
	var e struct {
		Title string `json:"title"`
		IDs   ids    `json:"ids"`
	}
	if err := get(ctx, fmt.Sprintf("/shows/%s/seasons/%d/episodes/%d", url.PathEscape(show), season, number), nil, &e); err != nil {
		return 0, fmt.Errorf("looking up %s S%02dE%02d (pick another show with -show): %w", show, season, number, err)
	}
	fmt.Printf("Episode: %s S%02dE%02d %q, trakt id %d\n", show, season, number, e.Title, e.IDs.Trakt)
	return e.IDs.Trakt, nil
}

// addPlay adds one play and returns Trakt's added and not_found counts.
func addPlay(ctx context.Context, kind string, id int64, watchedAt string) (string, error) {
	item := map[string]any{"ids": ids{Trakt: id}, "watched_at": watchedAt}
	var answer struct {
		Added    map[string]int             `json:"added"`
		NotFound map[string]json.RawMessage `json:"not_found"`
	}
	raw, err := write(ctx, "/sync/history", map[string]any{kind: []any{item}}, &answer)
	if err != nil {
		return "", err
	}
	if answer.Added[kind] != 1 {
		return "", fmt.Errorf("Trakt added %d %s: %s", answer.Added[kind], kind, raw)
	}
	return fmt.Sprintf("added %d", answer.Added[kind]), nil
}

// cleanup removes every play this run added, by history id.
func cleanup(ctx context.Context) {
	if len(added) == 0 || token == "" {
		return
	}
	slices.Sort(added)
	added = slices.Compact(added)
	var answer struct {
		Deleted  map[string]int `json:"deleted"`
		NotFound struct {
			IDs []int64 `json:"ids"`
		} `json:"not_found"`
	}
	fmt.Fprintf(&report, "\n## Cleanup\n\n")
	raw, err := write(ctx, "/sync/history/remove", map[string]any{"ids": added}, &answer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Removing the plays %v failed: %v\nRemove them by hand in the Trakt web app.\n", added, err)
		fmt.Fprintf(&report, "Removing the plays %v failed: %s. They need removing by hand.\n", added, oneLine(err.Error()))
		return
	}
	fmt.Printf("Removed the plays this run added (%v): %s\n", added, raw)
	fmt.Fprintf(&report, "Removed the plays this run added by history id (%v). Trakt answered `%s`.\n", added, raw)
}

// itemHistory is the user's history of one movie or episode.
func itemHistory(ctx context.Context, kind string, id int64) ([]entry, error) {
	return history(ctx, fmt.Sprintf("/sync/history/%s/%d", kind, id), url.Values{"limit": {"100"}})
}

// windowIDs are the history ids in the last 14 days, as the sync will ask.
func windowIDs(ctx context.Context) (map[int64]bool, error) {
	now := time.Now().UTC()
	q := url.Values{
		"start_at": {now.Add(-14 * 24 * time.Hour).Format(time.RFC3339)},
		"end_at":   {now.Format(time.RFC3339)},
		"limit":    {"1000"},
	}
	es, err := history(ctx, "/sync/history", q)
	if err != nil {
		return nil, err
	}
	m := map[int64]bool{}
	for _, e := range es {
		m[e.ID] = true
	}
	return m, nil
}

func history(ctx context.Context, path string, q url.Values) ([]entry, error) {
	var raws []json.RawMessage
	if err := get(ctx, path, q, &raws); err != nil {
		return nil, err
	}
	es := make([]entry, len(raws))
	for i, r := range raws {
		if err := json.Unmarshal(r, &es[i]); err != nil {
			return nil, err
		}
		es[i].Raw = r
	}
	return es, nil
}

func newEntries(old []int64, after []entry) []entry {
	var news []entry
	for _, e := range after {
		if !slices.Contains(old, e.ID) {
			news = append(news, e)
		}
	}
	return news
}

func idsOf(es []entry) []int64 {
	var out []int64
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func watchedAt(e entry) string {
	if e.WatchedAt == nil {
		return "null"
	}
	return *e.WatchedAt
}

func get(ctx context.Context, path string, q url.Values, out any) error {
	u := apiURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	status, body, err := do(ctx, http.MethodGet, u, nil, true)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, status, body)
	}
	return json.Unmarshal(body, out)
}

// write POSTs to the API, keeping to Trakt's one write a second.
func write(ctx context.Context, path string, in, out any) (string, error) {
	if d := time.Until(lastWrite.Add(1500 * time.Millisecond)); d > 0 {
		time.Sleep(d)
	}
	defer func() { lastWrite = time.Now() }()
	status, body, err := do(ctx, http.MethodPost, apiURL+path, in, true)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("POST %s: %d %s", path, status, body)
	}
	return string(bytes.TrimSpace(body)), json.Unmarshal(body, out)
}

func do(ctx context.Context, method, u string, in any, authed bool) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if in != nil {
			b, err := json.Marshal(in)
			if err != nil {
				return 0, nil, err
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("trakt-api-key", clientID)
		req.Header.Set("trakt-api-version", "2")
		req.Header.Set("User-Agent", "trakt-micropub-probe/1")
		if authed && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 && !strings.Contains(u, "/oauth/device/token") {
			wait := 5 * time.Second
			if s := resp.Header.Get("Retry-After"); s != "" {
				if d, err := time.ParseDuration(s + "s"); err == nil {
					wait = d + time.Second
				}
			}
			time.Sleep(wait)
			continue
		}
		return resp.StatusCode, bytes.TrimSpace(body), nil
	}
}

func indent(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return string(raw)
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", "\\|")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "probe:", err)
	os.Exit(1)
}
