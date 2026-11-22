-- Subtitle coverage that the Jellyfin plugin reads from Jellyfin's probed media
-- streams. There is one row per playable media source, so each edition of a
-- title is evaluated separately. 'present' means at least one selectable
-- subtitle stream in any language, including forced-only and unknown-language
-- tracks. 'missing' means the file was probed and has none. 'unknown' means
-- Jellyfin has not probed the file yet. fingerprint changes when the file is
-- replaced. sync_token marks the last coverage pass that saw the source; a
-- completed pass removes sources that it did not see.
CREATE TABLE subtitle_coverage (
    server_id          TEXT NOT NULL CHECK (server_id <> ''),
    media_source_id    TEXT NOT NULL CHECK (media_source_id <> ''),
    item_id            TEXT NOT NULL CHECK (item_id <> ''),
    media_type         TEXT NOT NULL CHECK (media_type IN ('movie', 'episode')),
    library            TEXT NOT NULL DEFAULT '',
    title              TEXT NOT NULL,
    series_title       TEXT NOT NULL DEFAULT '',
    season             INTEGER,
    episode            INTEGER,
    year               INTEGER NOT NULL DEFAULT 0,
    edition            TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL CHECK (status IN ('present', 'missing', 'unknown')),
    subtitle_languages TEXT NOT NULL DEFAULT '[]',
    fingerprint        TEXT NOT NULL,
    sync_token         TEXT NOT NULL,
    first_seen_at      TEXT NOT NULL,
    checked_at         TEXT NOT NULL,
    PRIMARY KEY (server_id, media_source_id)
) STRICT;

CREATE INDEX idx_subtitle_coverage_status ON subtitle_coverage (server_id, status, media_type);

-- One subtitle download per (server, media source, target language), shared by
-- everyone who asks for it. The plugin leases due jobs, searches the providers
-- configured in Jellyfin, and reports the outcome. A job counts as confirmed
-- only once Jellyfin lists the new track. detail is raw provider or filesystem
-- text for operators only. candidates holds weak matches for manual selection,
-- and subtitle_id is the candidate an operator chose. next_attempt_at is NULL
-- when nothing is scheduled.
CREATE TABLE subtitle_jobs (
    id              INTEGER PRIMARY KEY,
    server_id       TEXT NOT NULL,
    media_source_id TEXT NOT NULL,
    language        TEXT NOT NULL CHECK (length(language) = 3),
    item_id         TEXT NOT NULL,
    fingerprint     TEXT NOT NULL,
    origin          TEXT NOT NULL CHECK (origin IN ('backfill', 'import')),
    state           TEXT NOT NULL CHECK (state IN ('queued', 'running', 'waiting', 'confirmed', 'no_match', 'needs_review', 'failed', 'covered', 'stale', 'cancelled')),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    checks          INTEGER NOT NULL DEFAULT 0 CHECK (checks >= 0),
    subtitle_id     TEXT NOT NULL DEFAULT '',
    candidates      TEXT NOT NULL DEFAULT '[]',
    outcome         TEXT NOT NULL DEFAULT '',
    detail          TEXT NOT NULL DEFAULT '',
    provider        TEXT NOT NULL DEFAULT '',
    next_attempt_at TEXT,
    lease_until     TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (server_id, media_source_id, language)
) STRICT;

CREATE INDEX idx_subtitle_jobs_due ON subtitle_jobs (server_id, next_attempt_at) WHERE next_attempt_at IS NOT NULL;
CREATE INDEX idx_subtitle_jobs_state ON subtitle_jobs (server_id, state);
