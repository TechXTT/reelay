-- Household login sessions. Only the SHA-256 of the random token is stored, so
-- a copied database file cannot be replayed as a cookie. No foreign key to
-- jellyfin_users: operators sign in as Jellyfin administrators and may never
-- have been synced as members.
CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY,
    role         TEXT NOT NULL CHECK (role IN ('operator', 'member')),
    server_id    TEXT NOT NULL,
    user_id      TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
) STRICT;

CREATE INDEX idx_sessions_expires ON sessions (expires_at);
