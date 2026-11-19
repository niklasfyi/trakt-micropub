// Package trakt talks to the Trakt API: the device login, the rotating token
// pair, and the authenticated requests the sync makes.
package trakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ErrLoginRequired means the stored login can't be used any more: there is
// none, or Trakt refused to refresh it. Only running `login` again fixes it,
// so the client stops.
var ErrLoginRequired = errors.New("Trakt login required")

// Token is the pair Trakt hands out at login and on every refresh.
type Token struct {
	Access    string
	Refresh   string
	ExpiresAt time.Time
}

// TokenStore keeps the token pair.
type TokenStore interface {
	// Token returns the stored pair. With no pair stored it answers an
	// error wrapping ErrLoginRequired.
	Token(ctx context.Context) (Token, error)
	// SaveToken replaces the stored pair.
	SaveToken(ctx context.Context, t Token) error
	// RotateToken calls exchange with the stored pair inside a write
	// transaction and stores the pair it returns in that same transaction.
	// When exchange fails, the stored pair stays as it was. exchange must
	// not call the store.
	RotateToken(ctx context.Context, exchange func(old Token) (Token, error)) (Token, error)
}

// Config is what a Client needs.
type Config struct {
	ClientID string
	// ClientSecret is optional. Trakt has deprecated it, but a server may
	// still send it.
	ClientSecret string
	// APIURL is the API host, https://api.trakt.tv.
	APIURL string
	// AuthURL is the host for every OAuth request, https://auth.trakt.tv.
	AuthURL string
	// Version goes into the User-Agent.
	Version string
	Store   TokenStore
	// HTTP defaults to a client with a 30 second timeout.
	HTTP *http.Client
}

// RedirectURI is the redirect URI of an app without a callback, which is what
// a device-flow app registers.
const RedirectURI = "urn:ietf:wg:oauth:2.0:oob"

const (
	// refreshMargin is how long before it expires an access token is
	// refreshed, so a request doesn't race the expiry.
	refreshMargin = 10 * time.Minute
	// maxRetries is how many 429s in a row one request waits out.
	maxRetries = 5
	// maxBackoff caps the wait after a 429 without Retry-After.
	maxBackoff = time.Minute
)

// Client is a Trakt API client for one user.
type Client struct {
	cfg  Config
	http *http.Client
	// gets keeps authenticated GETs under Trakt's 500 per 5 minutes.
	gets *rate.Limiter
	// refreshing makes concurrent requests share one refresh.
	refreshing sync.Mutex

	// now and sleep are replaced in tests.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// New returns a Client.
func New(cfg Config) *Client {
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: 30 * time.Second}
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	cfg.AuthURL = strings.TrimRight(cfg.AuthURL, "/")
	return &Client{
		cfg:   cfg,
		http:  h,
		gets:  rate.NewLimiter(rate.Every(5*time.Minute/500), 500),
		now:   time.Now,
		sleep: sleepCtx,
	}
}

// StatusError is an answer from Trakt with an unexpected status.
type StatusError struct {
	Method, URL string
	Status      int
	Body        string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("trakt: %s %s: %d %s", e.Method, e.URL, e.Status, e.Body)
}

// LastActivities is the subset of /sync/last_activities the sync polls.
type LastActivities struct {
	All    time.Time `json:"all"`
	Movies struct {
		WatchedAt time.Time `json:"watched_at"`
		RatedAt   time.Time `json:"rated_at"`
	} `json:"movies"`
	Episodes struct {
		WatchedAt time.Time `json:"watched_at"`
		RatedAt   time.Time `json:"rated_at"`
	} `json:"episodes"`
}

// LastActivities returns the user's activity stamps.
func (c *Client) LastActivities(ctx context.Context) (LastActivities, error) {
	var la LastActivities
	err := c.Get(ctx, "/sync/last_activities", nil, &la)
	return la, err
}

// Get sends an authenticated GET to path on the API host and decodes the
// JSON answer into out.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.cfg.APIURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	resp, err := c.authed(ctx, http.MethodGet, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("trakt: GET %s: decoding: %w", u, err)
	}
	return nil
}

// authed sends an authenticated request and returns a 2xx answer. It
// refreshes the token pair when it is about to expire, and once more when
// Trakt answers 401.
func (c *Client) authed(ctx context.Context, method, u string) (*http.Response, error) {
	tok, err := c.cfg.Store.Token(ctx)
	if err != nil {
		return nil, err
	}
	if !c.now().Before(tok.ExpiresAt.Add(-refreshMargin)) {
		if tok, err = c.refresh(ctx, tok); err != nil {
			return nil, err
		}
	}
	for refreshed := false; ; refreshed = true {
		resp, err := c.send(ctx, func() (*http.Request, error) {
			if method == http.MethodGet {
				if err := c.gets.Wait(ctx); err != nil {
					return nil, err
				}
			}
			req, err := c.newRequest(ctx, method, u, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+tok.Access)
			return req, nil
		})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			if refreshed {
				return nil, fmt.Errorf("%w: Trakt refused a freshly refreshed access token", ErrLoginRequired)
			}
			if tok, err = c.refresh(ctx, tok); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return nil, statusError(resp)
		}
		return resp, nil
	}
}

// refresh trades the refresh token for a new pair. The new pair is stored in
// the transaction that receives it, before anything uses it.
func (c *Client) refresh(ctx context.Context, stale Token) (Token, error) {
	c.refreshing.Lock()
	defer c.refreshing.Unlock()
	return c.cfg.Store.RotateToken(ctx, func(cur Token) (Token, error) {
		if cur.Access != stale.Access {
			// Another request refreshed while this one waited.
			return cur, nil
		}
		return c.exchangeRefresh(ctx, cur.Refresh)
	})
}

func (c *Client) exchangeRefresh(ctx context.Context, refreshToken string) (Token, error) {
	body := map[string]string{
		"refresh_token": refreshToken,
		"client_id":     c.cfg.ClientID,
		"redirect_uri":  RedirectURI,
		"grant_type":    "refresh_token",
	}
	if c.cfg.ClientSecret != "" {
		body["client_secret"] = c.cfg.ClientSecret
	}
	u := c.cfg.AuthURL + "/oauth/token"
	resp, err := c.send(ctx, func() (*http.Request, error) { return c.newRequest(ctx, http.MethodPost, u, body) })
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		tok, err := c.decodeToken(resp.Body)
		if err != nil {
			// Cloudflare can answer with a 200 captcha page.
			return Token{}, fmt.Errorf("%w: the refresh answer isn't a token (%v)", ErrLoginRequired, err)
		}
		return tok, nil
	case resp.StatusCode/100 == 4:
		// 400 invalid_grant and its kin: the refresh token is spent or
		// revoked.
		return Token{}, fmt.Errorf("%w: Trakt refused the refresh: %w", ErrLoginRequired, statusError(resp))
	default:
		// A 5xx may be passing; the next pass tries again with the same
		// refresh token.
		return Token{}, statusError(resp)
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	CreatedAt    int64  `json:"created_at"`
}

// decodeToken reads a token answer. The lifetime comes from expires_in,
// counted from created_at when Trakt sends it.
func (c *Client) decodeToken(r io.Reader) (Token, error) {
	var tr tokenResponse
	if err := json.NewDecoder(r).Decode(&tr); err != nil {
		return Token{}, err
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" || tr.ExpiresIn <= 0 {
		return Token{}, errors.New("access_token, refresh_token or expires_in missing")
	}
	issued := c.now()
	if tr.CreatedAt > 0 {
		issued = time.Unix(tr.CreatedAt, 0)
	}
	return Token{
		Access:    tr.AccessToken,
		Refresh:   tr.RefreshToken,
		ExpiresAt: issued.Add(time.Duration(tr.ExpiresIn) * time.Second).UTC(),
	}, nil
}

// newRequest makes a request with the headers Trakt wants on every call.
func (c *Client) newRequest(ctx context.Context, method, u string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("trakt-api-key", c.cfg.ClientID)
	req.Header.Set("trakt-api-version", "2")
	req.Header.Set("User-Agent", "trakt-micropub/"+c.cfg.Version)
	return req, nil
}

// send sends the request build makes and waits out 429s: for Retry-After
// when Trakt sends it, otherwise with exponential backoff and jitter. build
// runs again for every attempt.
func (c *Client) send(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("trakt: %s %s: %w", req.Method, req.URL, err)
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		if attempt == maxRetries {
			return nil, statusError(resp)
		}
		wait := retryAfter(resp.Header.Get("Retry-After"), c.now())
		if wait < 0 {
			wait = backoff(attempt)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// retryAfter reads a Retry-After header, in seconds or as an HTTP date. It
// answers -1 when the header is missing or unreadable.
func retryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return -1
	}
	if s, err := strconv.Atoi(h); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(t.Sub(now), 0)
	}
	return -1
}

func backoff(attempt int) time.Duration {
	d := min(time.Second<<attempt, maxBackoff)
	return d/2 + rand.N(d/2+1)
}

func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	resp.Body.Close()
	return &StatusError{
		Method: resp.Request.Method,
		URL:    resp.Request.URL.String(),
		Status: resp.StatusCode,
		Body:   strings.TrimSpace(string(b)),
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
