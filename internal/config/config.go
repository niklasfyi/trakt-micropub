// Package config reads the client's settings from the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is every setting the client reads.
type Config struct {
	// TraktClientID is the API app's client id. TRAKT_CLIENT_ID.
	TraktClientID string
	// TraktClientSecret is optional; Trakt has deprecated it.
	// TRAKT_CLIENT_SECRET.
	TraktClientSecret string
	// TraktAPIURL is the API host. TRAKT_API_URL, https://api.trakt.tv by
	// default.
	TraktAPIURL string
	// TraktAuthURL is the host for OAuth. TRAKT_AUTH_URL,
	// https://auth.trakt.tv by default.
	TraktAuthURL string

	// MicropubEndpoint is the site's Micropub endpoint. MICROPUB_ENDPOINT.
	MicropubEndpoint string
	// MicropubToken needs the scopes create, update, delete and read.
	// MICROPUB_TOKEN.
	MicropubToken string
	// SiteURL is the site the posts go to. SITE_URL.
	SiteURL string

	// TMDBKey is the TMDB API key, for posters. TMDB_API_KEY.
	TMDBKey string

	// TimeZone is the zone TV days are counted in. TIME_ZONE, Europe/Oslo
	// by default.
	TimeZone *time.Location
	// PollInterval is how often `run` polls Trakt. POLL_INTERVAL as a Go
	// duration, 15m by default.
	PollInterval time.Duration
	// StatePath is the SQLite state file. STATE_PATH, trakt-micropub.db by
	// default.
	StatePath string
}

// Need says which settings a command requires.
type Need int

const (
	// ForLogin needs only what the Trakt login uses.
	ForLogin Need = iota
	// ForSync needs everything.
	ForSync
)

// Load reads the settings through getenv and checks that need's are set.
func Load(getenv func(string) string, need Need) (Config, error) {
	get := func(name, fallback string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	c := Config{
		TraktClientID:     get("TRAKT_CLIENT_ID", ""),
		TraktClientSecret: get("TRAKT_CLIENT_SECRET", ""),
		TraktAPIURL:       get("TRAKT_API_URL", "https://api.trakt.tv"),
		TraktAuthURL:      get("TRAKT_AUTH_URL", "https://auth.trakt.tv"),
		MicropubEndpoint:  get("MICROPUB_ENDPOINT", ""),
		MicropubToken:     get("MICROPUB_TOKEN", ""),
		SiteURL:           get("SITE_URL", ""),
		TMDBKey:           get("TMDB_API_KEY", ""),
		StatePath:         get("STATE_PATH", "trakt-micropub.db"),
	}

	var missing []string
	var problems []error
	required := map[string]string{"TRAKT_CLIENT_ID": c.TraktClientID}
	if need == ForSync {
		required["MICROPUB_ENDPOINT"] = c.MicropubEndpoint
		required["MICROPUB_TOKEN"] = c.MicropubToken
		required["SITE_URL"] = c.SiteURL
		required["TMDB_API_KEY"] = c.TMDBKey
	}
	for _, name := range []string{"TRAKT_CLIENT_ID", "MICROPUB_ENDPOINT", "MICROPUB_TOKEN", "SITE_URL", "TMDB_API_KEY"} {
		if v, ok := required[name]; ok && v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Errorf("missing %s", strings.Join(missing, ", ")))
	}

	for _, u := range []struct{ name, value string }{
		{"TRAKT_API_URL", c.TraktAPIURL},
		{"TRAKT_AUTH_URL", c.TraktAuthURL},
		{"MICROPUB_ENDPOINT", c.MicropubEndpoint},
		{"SITE_URL", c.SiteURL},
	} {
		if u.value == "" {
			continue
		}
		if p, err := url.Parse(u.value); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			problems = append(problems, fmt.Errorf("%s is not an http(s) URL: %q", u.name, u.value))
		}
	}

	zone := get("TIME_ZONE", "Europe/Oslo")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		problems = append(problems, fmt.Errorf("TIME_ZONE %q: %w", zone, err))
	}
	c.TimeZone = loc

	interval := get("POLL_INTERVAL", "15m")
	d, err := time.ParseDuration(interval)
	switch {
	case err != nil:
		problems = append(problems, fmt.Errorf("POLL_INTERVAL %q is not a duration such as 15m", interval))
	case d < time.Minute:
		problems = append(problems, fmt.Errorf("POLL_INTERVAL %q is under a minute", interval))
	}
	c.PollInterval = d

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("config: %w", errors.Join(problems...))
	}
	return c, nil
}
