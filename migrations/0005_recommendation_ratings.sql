-- Explicit per-user ratings and rating activity from Jellyfin.

CREATE TABLE jellyfin_activity_new (
    event_id        TEXT PRIMARY KEY,
    server_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    item_id         TEXT NOT NULL,
    event_type      TEXT NOT NULL CHECK (event_type IN ('played', 'completed', 'favorite', 'like', 'dislike', 'abandoned', 'rating')),
    progress        REAL NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 1),
    occurred_at     TEXT NOT NULL,
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

INSERT INTO jellyfin_activity_new(event_id,server_id,user_id,item_id,event_type,progress,occurred_at)
SELECT event_id,server_id,user_id,item_id,event_type,progress,occurred_at FROM jellyfin_activity;

DROP TABLE jellyfin_activity;
ALTER TABLE jellyfin_activity_new RENAME TO jellyfin_activity;
CREATE INDEX idx_jellyfin_activity_user ON jellyfin_activity (server_id, user_id, occurred_at DESC);

CREATE TABLE recommendation_ratings (
    id              INTEGER PRIMARY KEY,
    action_id       TEXT NOT NULL UNIQUE,
    server_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    media_type      TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    tmdb_id         INTEGER NOT NULL CHECK (tmdb_id > 0),
    rating          INTEGER NOT NULL CHECK (rating BETWEEN 1 AND 5),
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (server_id, user_id, media_type, tmdb_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_recommendation_ratings_user
ON recommendation_ratings (server_id, user_id, media_type, updated_at DESC);
