-- Durable requester attribution survives recommendation pruning and expiry.
CREATE TABLE media_requests (
    id            INTEGER PRIMARY KEY,
    server_id     TEXT NOT NULL,
    user_id       TEXT NOT NULL,
    media_type    TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    tmdb_id       INTEGER NOT NULL CHECK (tmdb_id > 0),
    title         TEXT NOT NULL,
    year          INTEGER NOT NULL DEFAULT 0 CHECK (year >= 0),
    requested_at  TEXT NOT NULL,
    monitor_mode  TEXT NOT NULL DEFAULT '' CHECK (monitor_mode IN ('', 'latest_season', 'all', 'future_only')),
    subject_type  TEXT NOT NULL CHECK (subject_type IN ('movie', 'series')),
    subject_id    INTEGER NOT NULL CHECK (subject_id > 0),
    UNIQUE (server_id, user_id, media_type, tmdb_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_media_requests_user ON media_requests (server_id, user_id, requested_at DESC);

-- Preserve requesters from installations upgrading after recommendation feedback was introduced.
INSERT OR IGNORE INTO media_requests(server_id,user_id,media_type,tmdb_id,title,year,requested_at,monitor_mode,subject_type,subject_id)
SELECT f.server_id,f.user_id,f.media_type,f.tmdb_id,m.title,m.year,f.created_at,'','movie',m.id
FROM recommendation_feedback f JOIN movies m ON f.media_type='movie' AND m.tmdb_id=f.tmdb_id
WHERE f.action='request'
UNION ALL
SELECT f.server_id,f.user_id,f.media_type,f.tmdb_id,s.title,s.year,f.created_at,'','series',s.id
FROM recommendation_feedback f JOIN series s ON f.media_type='series' AND s.tmdb_id=f.tmdb_id
WHERE f.action='request';
