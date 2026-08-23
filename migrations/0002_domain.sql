-- 0002_domain.sql
-- Phase 6: the durable domain model and lifecycle audit.

CREATE TABLE quality_profiles (
    id                       INTEGER PRIMARY KEY,
    name                     TEXT NOT NULL COLLATE NOCASE UNIQUE,
    is_default               INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    allowed_resolutions_json TEXT NOT NULL CHECK (json_valid(allowed_resolutions_json)),
    allowed_sources_json     TEXT NOT NULL CHECK (json_valid(allowed_sources_json)),
    min_size_mb              INTEGER NOT NULL CHECK (min_size_mb >= 0),
    max_size_mb              INTEGER NOT NULL CHECK (max_size_mb >= min_size_mb),
    min_seeders              INTEGER NOT NULL CHECK (min_seeders >= 0),
    required_terms_json      TEXT NOT NULL CHECK (json_valid(required_terms_json)),
    banned_terms_json        TEXT NOT NULL CHECK (json_valid(banned_terms_json)),
    preferred_groups_json    TEXT NOT NULL CHECK (json_valid(preferred_groups_json)),
    language_prefs_json      TEXT NOT NULL CHECK (json_valid(language_prefs_json)),
    hdr_prefs_json           TEXT NOT NULL CHECK (json_valid(hdr_prefs_json)),
    upgrade_until            TEXT,
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_quality_profiles_one_default
    ON quality_profiles (is_default) WHERE is_default = 1;

CREATE TABLE series (
    id                    INTEGER PRIMARY KEY,
    title                 TEXT NOT NULL,
    sort_title            TEXT NOT NULL,
    year                  INTEGER NOT NULL DEFAULT 0 CHECK (year >= 0),
    aliases_json          TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(aliases_json)),
    tvmaze_id             INTEGER NOT NULL DEFAULT 0 CHECK (tvmaze_id >= 0),
    tmdb_id               INTEGER NOT NULL DEFAULT 0 CHECK (tmdb_id >= 0),
    imdb_id               TEXT NOT NULL DEFAULT '',
    is_anime              INTEGER NOT NULL DEFAULT 0 CHECK (is_anime IN (0, 1)),
    absolute_offset       INTEGER NOT NULL DEFAULT 0,
    monitor_mode          TEXT NOT NULL CHECK (monitor_mode IN ('all', 'future_only', 'latest_season', 'none')),
    quality_profile_id    INTEGER NOT NULL REFERENCES quality_profiles(id) ON DELETE RESTRICT,
    root_folder           TEXT NOT NULL,
    status                TEXT NOT NULL CHECK (status IN ('following', 'paused', 'ended')),
    runtime_minutes       INTEGER NOT NULL DEFAULT 0 CHECK (runtime_minutes >= 0),
    added_at              TEXT NOT NULL,
    episodes_refreshed_at TEXT
) STRICT;

CREATE UNIQUE INDEX idx_series_tvmaze_id ON series (tvmaze_id) WHERE tvmaze_id > 0;
CREATE INDEX idx_series_status ON series (status, sort_title);

CREATE TABLE releases (
    id           INTEGER PRIMARY KEY,
    indexer      TEXT NOT NULL,
    raw_title    TEXT NOT NULL,
    info_hash    TEXT NOT NULL COLLATE NOCASE,
    magnet       TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL CHECK (size_bytes >= 0),
    seeders      INTEGER NOT NULL CHECK (seeders >= 0),
    leechers     INTEGER NOT NULL DEFAULT 0 CHECK (leechers >= 0),
    published_at TEXT,
    category     INTEGER NOT NULL DEFAULT 0,
    parsed_json  TEXT NOT NULL CHECK (json_valid(parsed_json)),
    score        INTEGER NOT NULL DEFAULT 0,
    seen_at      TEXT NOT NULL,
    UNIQUE (indexer, info_hash)
) STRICT;

CREATE INDEX idx_releases_hash ON releases (info_hash);
CREATE INDEX idx_releases_seen ON releases (seen_at DESC);

CREATE TABLE episodes (
    id                 INTEGER PRIMARY KEY,
    series_id          INTEGER NOT NULL REFERENCES series(id) ON DELETE CASCADE,
    season             INTEGER NOT NULL CHECK (season >= 0),
    number             INTEGER NOT NULL CHECK (number >= 0),
    absolute_number    INTEGER NOT NULL DEFAULT 0 CHECK (absolute_number >= 0),
    title              TEXT NOT NULL DEFAULT '',
    air_date           TEXT,
    state              TEXT NOT NULL CHECK (state IN ('unmonitored', 'wanted', 'searching', 'grabbed', 'downloading', 'importing', 'imported', 'import_failed', 'failed')),
    chosen_release_id  INTEGER REFERENCES releases(id) ON DELETE SET NULL,
    imported_path      TEXT NOT NULL DEFAULT '',
    imported_quality   TEXT NOT NULL DEFAULT '',
    search_attempts    INTEGER NOT NULL DEFAULT 0 CHECK (search_attempts >= 0),
    next_search_at     TEXT,
    first_wanted_at    TEXT,
    last_error         TEXT NOT NULL DEFAULT '',
    UNIQUE (series_id, season, number)
) STRICT;

CREATE INDEX idx_episodes_state_search ON episodes (state, next_search_at);
CREATE INDEX idx_episodes_series_number ON episodes (series_id, season, number);
CREATE INDEX idx_episodes_air_date ON episodes (air_date);

CREATE TABLE movies (
    id                 INTEGER PRIMARY KEY,
    title              TEXT NOT NULL,
    sort_title         TEXT NOT NULL,
    year               INTEGER NOT NULL DEFAULT 0 CHECK (year >= 0),
    tmdb_id             INTEGER NOT NULL DEFAULT 0 CHECK (tmdb_id >= 0),
    imdb_id             TEXT NOT NULL DEFAULT '',
    runtime_minutes     INTEGER NOT NULL DEFAULT 0 CHECK (runtime_minutes >= 0),
    quality_profile_id INTEGER NOT NULL REFERENCES quality_profiles(id) ON DELETE RESTRICT,
    root_folder         TEXT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN ('unmonitored', 'wanted', 'searching', 'grabbed', 'downloading', 'importing', 'imported', 'import_failed', 'failed')),
    chosen_release_id  INTEGER REFERENCES releases(id) ON DELETE SET NULL,
    imported_path      TEXT NOT NULL DEFAULT '',
    imported_quality   TEXT NOT NULL DEFAULT '',
    search_attempts    INTEGER NOT NULL DEFAULT 0 CHECK (search_attempts >= 0),
    next_search_at     TEXT,
    first_wanted_at    TEXT,
    last_error         TEXT NOT NULL DEFAULT '',
    added_at           TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX idx_movies_tmdb_id ON movies (tmdb_id) WHERE tmdb_id > 0;
CREATE INDEX idx_movies_state_search ON movies (state, next_search_at);
CREATE INDEX idx_movies_sort_title ON movies (sort_title, year);

CREATE TABLE candidate_evaluations (
    id           INTEGER PRIMARY KEY,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('episode', 'movie')),
    subject_id   INTEGER NOT NULL CHECK (subject_id > 0),
    release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    accepted     INTEGER NOT NULL CHECK (accepted IN (0, 1)),
    reason_code  TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    score        INTEGER NOT NULL DEFAULT 0,
    evaluated_at TEXT NOT NULL
) STRICT;

CREATE INDEX idx_candidate_subject
    ON candidate_evaluations (subject_type, subject_id, evaluated_at DESC);

CREATE TABLE grabs (
    id            INTEGER PRIMARY KEY,
    subject_type  TEXT NOT NULL CHECK (subject_type IN ('episode', 'movie')),
    subject_id    INTEGER NOT NULL CHECK (subject_id > 0),
    release_id    INTEGER NOT NULL REFERENCES releases(id) ON DELETE RESTRICT,
    torrent_hash  TEXT NOT NULL COLLATE NOCASE,
    category      TEXT NOT NULL,
    state         TEXT NOT NULL CHECK (state IN ('pending', 'downloading', 'completed', 'importing', 'imported', 'stalled', 'removed', 'failed')),
    progress      REAL NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 1),
    content_path  TEXT NOT NULL DEFAULT '',
    attempts      INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    progressed_at TEXT
) STRICT;

CREATE INDEX idx_grabs_active ON grabs (state, updated_at);
CREATE INDEX idx_grabs_subject ON grabs (subject_type, subject_id, created_at DESC);
CREATE INDEX idx_grabs_hash ON grabs (torrent_hash);

CREATE TABLE release_blacklist (
    id           INTEGER PRIMARY KEY,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('episode', 'movie')),
    subject_id   INTEGER NOT NULL CHECK (subject_id > 0),
    info_hash    TEXT NOT NULL COLLATE NOCASE,
    reason       TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    UNIQUE (subject_type, subject_id, info_hash)
) STRICT;

CREATE INDEX idx_blacklist_subject ON release_blacklist (subject_type, subject_id);

CREATE TABLE imports (
    id            INTEGER PRIMARY KEY,
    grab_id       INTEGER NOT NULL REFERENCES grabs(id) ON DELETE RESTRICT,
    subject_type  TEXT NOT NULL CHECK (subject_type IN ('episode', 'movie')),
    subject_id    INTEGER NOT NULL CHECK (subject_id > 0),
    source_path   TEXT NOT NULL,
    dest_path     TEXT NOT NULL,
    method        TEXT NOT NULL CHECK (method IN ('hardlink', 'copy', 'move')),
    size_bytes    INTEGER NOT NULL CHECK (size_bytes >= 0),
    replaced_path TEXT NOT NULL DEFAULT '',
    imported_at   TEXT NOT NULL
) STRICT;

CREATE INDEX idx_imports_subject ON imports (subject_type, subject_id, imported_at DESC);

CREATE TABLE state_transitions (
    id           INTEGER PRIMARY KEY,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('episode', 'movie')),
    subject_id   INTEGER NOT NULL CHECK (subject_id > 0),
    from_state   TEXT CHECK (from_state IS NULL OR from_state IN ('unmonitored', 'wanted', 'searching', 'grabbed', 'downloading', 'importing', 'imported', 'import_failed', 'failed')),
    to_state     TEXT NOT NULL CHECK (to_state IN ('unmonitored', 'wanted', 'searching', 'grabbed', 'downloading', 'importing', 'imported', 'import_failed', 'failed')),
    reason       TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',
    transitioned_at TEXT NOT NULL
) STRICT;

CREATE INDEX idx_transitions_subject
    ON state_transitions (subject_type, subject_id, transitioned_at DESC, id DESC);
CREATE INDEX idx_transitions_recent ON state_transitions (transitioned_at DESC, id DESC);

-- Leases rather than permanent mutex rows: if a process dies while holding one,
-- another process can reclaim it after expires_at.
CREATE TABLE item_locks (
    subject_type TEXT NOT NULL CHECK (subject_type IN ('episode', 'movie')),
    subject_id   INTEGER NOT NULL CHECK (subject_id > 0),
    owner        TEXT NOT NULL,
    acquired_at  TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    PRIMARY KEY (subject_type, subject_id)
) STRICT;

CREATE INDEX idx_item_locks_expiry ON item_locks (expires_at);
