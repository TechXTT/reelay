-- 0001_infrastructure.sql
--
-- Phase 1: everything that is not the domain model. The domain model (series,
-- episodes, movies, releases, grabs, profiles, audit) arrives in 0002.
--
-- Conventions used throughout Reelay's schema:
--   * Timestamps are TEXT, RFC3339 in UTC, always 'YYYY-MM-DDTHH:MM:SSZ'.
--     Fixed-width UTC sorts lexicographically, so plain <, > and BETWEEN work,
--     and the values are readable in the sqlite3 CLI while debugging.
--   * Enumerations are TEXT with a CHECK constraint. A typo'd state is then a
--     write error rather than a row that no switch statement handles.
--   * Every table that grows has an explicit index for the way we query it.

-- Bookkeeping that outlives a restart but is not domain data: last run time
-- per scheduler loop, first-run markers, seeded-profiles flag.
CREATE TABLE kv (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Cached metadata provider responses. Keyed by provider + request key so a
-- TMDB search and a TVmaze episode list never collide.
--
-- `payload` is the raw provider response body. We re-decode on read rather
-- than storing our own projection: providers add fields, and re-fetching every
-- series because we widened a struct is a worse trade than a decode per read.
CREATE TABLE metadata_cache (
    provider   TEXT NOT NULL,
    key        TEXT NOT NULL,
    payload    BLOB NOT NULL,
    fetched_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    PRIMARY KEY (provider, key)
) STRICT;

CREATE INDEX idx_metadata_cache_expiry ON metadata_cache (expires_at);

-- Circuit breaker state, persisted so a restart does not immediately hammer an
-- indexer we had already decided was down.
CREATE TABLE indexer_health (
    name                 TEXT PRIMARY KEY,
    healthy              INTEGER NOT NULL DEFAULT 1 CHECK (healthy IN (0, 1)),
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    unhealthy_until      TEXT,
    last_error           TEXT,
    last_success_at      TEXT,
    checked_at           TEXT NOT NULL
) STRICT;
