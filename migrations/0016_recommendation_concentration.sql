-- Top-ten concentration of each generated recommendation list, so a narrowing
-- list shows up as a trend next to requests and later viewing. Shares are
-- fractions of the sample (at most ten titles).
CREATE TABLE recommendation_concentration (
    id                   INTEGER PRIMARY KEY,
    server_id            TEXT NOT NULL,
    user_id              TEXT NOT NULL,
    media_type           TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    generated_at         TEXT NOT NULL,
    sample_size          INTEGER NOT NULL CHECK (sample_size BETWEEN 1 AND 10),
    dominant_genre       TEXT NOT NULL,
    dominant_genre_share REAL NOT NULL CHECK (dominant_genre_share BETWEEN 0 AND 1),
    dominant_seed        TEXT NOT NULL,
    dominant_seed_share  REAL NOT NULL CHECK (dominant_seed_share BETWEEN 0 AND 1),
    distinct_interests   INTEGER NOT NULL CHECK (distinct_interests >= 1),
    distinct_genres      INTEGER NOT NULL CHECK (distinct_genres >= 0),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_recommendation_concentration_user
ON recommendation_concentration (server_id, user_id, media_type, generated_at);
