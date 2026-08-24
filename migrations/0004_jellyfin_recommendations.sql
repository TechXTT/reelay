-- Jellyfin integration and explainable per-user recommendations.

CREATE TABLE jellyfin_users (
    server_id        TEXT NOT NULL,
    user_id          TEXT NOT NULL,
    display_name     TEXT NOT NULL,
    enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    last_synced_at   TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    PRIMARY KEY (server_id, user_id)
) STRICT;

CREATE UNIQUE INDEX idx_series_tmdb_id ON series (tmdb_id) WHERE tmdb_id > 0;

CREATE TABLE jellyfin_items (
    server_id       TEXT NOT NULL,
    item_id         TEXT NOT NULL,
    media_type      TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    tmdb_id         INTEGER NOT NULL DEFAULT 0 CHECK (tmdb_id >= 0),
    tvdb_id         INTEGER NOT NULL DEFAULT 0 CHECK (tvdb_id >= 0),
    imdb_id         TEXT NOT NULL DEFAULT '',
    title           TEXT NOT NULL,
    year            INTEGER NOT NULL DEFAULT 0 CHECK (year >= 0),
    genres_json     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(genres_json)),
    keywords_json   TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(keywords_json)),
    people_json     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(people_json)),
    language        TEXT NOT NULL DEFAULT '',
    country         TEXT NOT NULL DEFAULT '',
    runtime_minutes INTEGER NOT NULL DEFAULT 0 CHECK (runtime_minutes >= 0),
    present         INTEGER NOT NULL DEFAULT 1 CHECK (present IN (0, 1)),
    sync_token      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (server_id, item_id)
) STRICT;

CREATE INDEX idx_jellyfin_items_tmdb ON jellyfin_items (server_id, media_type, tmdb_id);

CREATE TABLE jellyfin_activity (
    event_id        TEXT PRIMARY KEY,
    server_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    item_id         TEXT NOT NULL,
    event_type      TEXT NOT NULL CHECK (event_type IN ('played', 'completed', 'favorite', 'like', 'dislike', 'abandoned')),
    progress        REAL NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 1),
    occurred_at     TEXT NOT NULL,
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_jellyfin_activity_user ON jellyfin_activity (server_id, user_id, occurred_at DESC);

CREATE TABLE recommendations (
    id              INTEGER PRIMARY KEY,
    server_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    media_type      TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    tmdb_id         INTEGER NOT NULL CHECK (tmdb_id > 0),
    title           TEXT NOT NULL,
    year            INTEGER NOT NULL DEFAULT 0 CHECK (year >= 0),
    overview        TEXT NOT NULL DEFAULT '',
    poster_url      TEXT NOT NULL DEFAULT '',
    score           REAL NOT NULL CHECK (score >= 0 AND score <= 100),
    reasons_json    TEXT NOT NULL CHECK (json_valid(reasons_json)),
    features_json   TEXT NOT NULL CHECK (json_valid(features_json)),
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'dismissed', 'requested', 'available')),
    generated_at    TEXT NOT NULL,
    expires_at      TEXT NOT NULL,
    UNIQUE (server_id, user_id, media_type, tmdb_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_recommendations_user ON recommendations (server_id, user_id, media_type, status, score DESC);

CREATE TABLE recommendation_feedback (
    id              INTEGER PRIMARY KEY,
    action_id       TEXT NOT NULL UNIQUE,
    server_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    media_type      TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    tmdb_id         INTEGER NOT NULL CHECK (tmdb_id > 0),
    action          TEXT NOT NULL CHECK (action IN ('dismiss', 'request')),
    created_at      TEXT NOT NULL,
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_recommendation_feedback_user ON recommendation_feedback (server_id, user_id, action, created_at DESC);
