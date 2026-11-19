// Command trakt-micropub copies a Trakt watch history to a Micropub site.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
	_ "time/tzdata" // TIME_ZONE works in a container without zoneinfo.

	"github.com/niklasfyi/trakt-micropub/internal/config"
	"github.com/niklasfyi/trakt-micropub/internal/state"
	"github.com/niklasfyi/trakt-micropub/internal/trakt"
)

// version is set at build time with -ldflags "-X main.version=…".
var version string

const usage = `usage: trakt-micropub <command>

Commands:
  login   sign in to Trakt with a device code and store the tokens
  run     poll Trakt and sync until stopped
  once    run one sync pass and exit

Settings come from the environment; see the README.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	var err error
	switch args[0] {
	case "login":
		err = login(ctx, getenv, stdout)
	case "once":
		err = withSync(getenv, log, func(s *syncer) error { return s.pass(ctx) })
	case "run":
		err = withSync(getenv, log, func(s *syncer) error { return s.loop(ctx) })
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "trakt-micropub: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err == nil {
		return 0
	}
	switch {
	case errors.Is(err, trakt.ErrLoginRequired):
		fmt.Fprintf(stderr, "trakt-micropub: %v\nRun `trakt-micropub login` to sign in to Trakt again.\n", err)
	case errors.Is(err, state.ErrLocked):
		fmt.Fprintf(stderr, "trakt-micropub: %v\nOnly one trakt-micropub may use a state file at a time. Stop the other one first.\n", err)
	default:
		fmt.Fprintf(stderr, "trakt-micropub: %v\n", err)
	}
	return 1
}

func login(ctx context.Context, getenv func(string) string, stdout io.Writer) error {
	cfg, err := config.Load(getenv, config.ForLogin)
	if err != nil {
		return err
	}
	st, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer st.Close()
	c := newTrakt(cfg, st)
	err = c.Login(ctx, func(dc trakt.DeviceCode) {
		fmt.Fprintf(stdout, "Open %s and enter the code %s\nWaiting for approval…\n", dc.VerificationURL, dc.UserCode)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Signed in. The tokens are stored in %s.\n", cfg.StatePath)
	return nil
}

// syncer is what `run` and `once` share.
type syncer struct {
	cfg   config.Config
	trakt *trakt.Client
	log   *slog.Logger
}

func withSync(getenv func(string) string, log *slog.Logger, do func(*syncer) error) error {
	cfg, err := config.Load(getenv, config.ForSync)
	if err != nil {
		return err
	}
	st, err := state.Open(cfg.StatePath)
	if err != nil {
		return err
	}
	defer st.Close()
	return do(&syncer{cfg: cfg, trakt: newTrakt(cfg, st), log: log})
}

// pass is one sync pass. For now it only reads Trakt's activity stamps,
// which proves the login works.
func (s *syncer) pass(ctx context.Context) error {
	la, err := s.trakt.LastActivities(ctx)
	if err != nil {
		return err
	}
	s.log.Info("pass done", "trakt_last_activity", la.All.Format(time.RFC3339))
	return nil
}

// loop runs a pass every poll interval until ctx ends. A failed pass is
// logged and tried again next time, except a lost login, which stops it.
func (s *syncer) loop(ctx context.Context) error {
	s.log.Info("polling Trakt", "every", s.cfg.PollInterval.String())
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		if err := s.pass(ctx); err != nil {
			if errors.Is(err, trakt.ErrLoginRequired) {
				return err
			}
			if ctx.Err() == nil {
				s.log.Error("pass failed", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			s.log.Info("stopped")
			return nil
		case <-t.C:
		}
	}
}

func newTrakt(cfg config.Config, st *state.Store) *trakt.Client {
	return trakt.New(trakt.Config{
		ClientID:     cfg.TraktClientID,
		ClientSecret: cfg.TraktClientSecret,
		APIURL:       cfg.TraktAPIURL,
		AuthURL:      cfg.TraktAuthURL,
		Version:      buildVersion(),
		Store:        st,
	})
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
