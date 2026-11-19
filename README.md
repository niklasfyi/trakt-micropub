# trakt-micropub

Sync Trakt watch history to a Micropub endpoint.

Work in progress: so far it signs in to Trakt and keeps the login alive. Posting comes next.

## Setup

1. Create a Trakt API app at [app.trakt.tv/settings/apps](https://app.trakt.tv/settings/apps). Use `urn:ietf:wg:oauth:2.0:oob` as the redirect URI. From 2027-01-01 Trakt only approves apps whose account has a linked GitHub account. Keep a copy of the client id.
2. Set the environment below and run `trakt-micropub login`. Open the URL it prints, enter the code, and approve the app.
3. Run `trakt-micropub run`.

## Commands

- `login`: signs in to Trakt with a device code and stores the tokens.
- `run`: polls Trakt and syncs until stopped.
- `once`: runs one sync pass and exits.

Only one process may use a state file at a time. A second one exits with a message saying so.

If Trakt refuses to refresh the login, the client stops and asks you to run `login` again.

## Settings

| Variable | Needed for | Default | |
|---|---|---|---|
| `TRAKT_CLIENT_ID` | all | | The Trakt app's client id |
| `TRAKT_CLIENT_SECRET` | | | Optional. Trakt has deprecated it |
| `MICROPUB_ENDPOINT` | `run`, `once` | | The site's Micropub endpoint |
| `MICROPUB_TOKEN` | `run`, `once` | | A token with the scopes `create update delete read` |
| `SITE_URL` | `run`, `once` | | The site the posts go to |
| `TMDB_API_KEY` | `run`, `once` | | For posters |
| `TIME_ZONE` | | `Europe/Oslo` | The zone TV days are counted in |
| `POLL_INTERVAL` | | `15m` | How often `run` polls, as a Go duration |
| `STATE_PATH` | | `trakt-micropub.db` | The SQLite state file. A `.lock` file goes beside it |
| `TRAKT_API_URL` | | `https://api.trakt.tv` | |
| `TRAKT_AUTH_URL` | | `https://auth.trakt.tv` | |

## Build

```
go build -ldflags "-X main.version=$(git describe --tags --always)" ./cmd/trakt-micropub
```
