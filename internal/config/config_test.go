package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func full() map[string]string {
	return map[string]string{
		"TRAKT_CLIENT_ID":   "cid",
		"MICROPUB_ENDPOINT": "https://example.com/micropub",
		"MICROPUB_TOKEN":    "tok",
		"SITE_URL":          "https://example.com/",
		"TMDB_API_KEY":      "tmdb",
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(full()), ForSync)
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeZone.String() != "Europe/Oslo" || c.PollInterval != 15*time.Minute || c.StatePath != "trakt-micropub.db" {
		t.Errorf("defaults: zone %s, interval %s, state %s", c.TimeZone, c.PollInterval, c.StatePath)
	}
	if c.TraktAPIURL != "https://api.trakt.tv" || c.TraktAuthURL != "https://auth.trakt.tv" {
		t.Errorf("Trakt hosts: %s, %s", c.TraktAPIURL, c.TraktAuthURL)
	}
}

func TestLoginNeedsOnlyTrakt(t *testing.T) {
	if _, err := Load(env(map[string]string{"TRAKT_CLIENT_ID": "cid"}), ForLogin); err != nil {
		t.Fatal(err)
	}
}

func TestSyncNamesEveryMissingSetting(t *testing.T) {
	_, err := Load(env(map[string]string{"TRAKT_CLIENT_ID": "cid"}), ForSync)
	if err == nil {
		t.Fatal("no error")
	}
	for _, name := range []string{"MICROPUB_ENDPOINT", "MICROPUB_TOKEN", "SITE_URL", "TMDB_API_KEY"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%q doesn't name %s", err, name)
		}
	}
}

func TestBadValues(t *testing.T) {
	for name, value := range map[string]string{
		"TIME_ZONE":         "Mars/Olympus",
		"POLL_INTERVAL":     "often",
		"MICROPUB_ENDPOINT": "example.com/micropub",
	} {
		m := full()
		m[name] = value
		if _, err := Load(env(m), ForSync); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: err = %v", name, value, err)
		}
	}
}
