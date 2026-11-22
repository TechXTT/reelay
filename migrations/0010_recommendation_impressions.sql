-- First time each title was offered to a user, so recommendation outcomes can
-- be measured without a stored history of every refresh.
CREATE TABLE recommendation_impressions (
    server_id        TEXT NOT NULL,
    user_id          TEXT NOT NULL,
    media_type       TEXT NOT NULL CHECK (media_type IN ('movie', 'series')),
    tmdb_id          INTEGER NOT NULL CHECK (tmdb_id > 0),
    first_offered_at TEXT NOT NULL,
    last_offered_at  TEXT NOT NULL,
    best_rank        INTEGER NOT NULL CHECK (best_rank >= 1),
    first_score      REAL NOT NULL,
    PRIMARY KEY (server_id, user_id, media_type, tmdb_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_recommendation_impressions_user
ON recommendation_impressions (server_id, user_id, first_offered_at);

-- Titles currently stored were offered when they were generated. Only the best
-- 40 per user and media type are materialized by the Jellyfin plugin.
INSERT INTO recommendation_impressions(server_id,user_id,media_type,tmdb_id,first_offered_at,last_offered_at,best_rank,first_score)
SELECT server_id,user_id,media_type,tmdb_id,generated_at,generated_at,position,score
FROM (
    SELECT server_id,user_id,media_type,tmdb_id,generated_at,score,
           ROW_NUMBER() OVER (PARTITION BY server_id,user_id,media_type ORDER BY score DESC,tmdb_id) AS position
    FROM recommendations
)
WHERE position <= 40;
