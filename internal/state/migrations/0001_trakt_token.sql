-- The Trakt token pair. There is only ever one row.
CREATE TABLE trakt_token (
	id            INTEGER PRIMARY KEY CHECK (id = 1),
	access_token  TEXT NOT NULL,
	refresh_token TEXT NOT NULL,
	expires_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL
) STRICT;
