CREATE TABLE series_trials (
    request_id INTEGER PRIMARY KEY REFERENCES media_requests(id) ON DELETE CASCADE,
    scope TEXT NOT NULL CHECK (scope IN ('one_episode','three_episodes','first_season')),
    episode_limit INTEGER NOT NULL CHECK (episode_limit BETWEEN 0 AND 10000),
    season_number INTEGER,
    decision TEXT NOT NULL DEFAULT '' CHECK (decision IN ('','continue','stop')),
    rating INTEGER CHECK (rating BETWEEN 1 AND 5),
    voted_at TEXT
) STRICT;

CREATE TABLE series_trial_episodes (
    request_id INTEGER NOT NULL REFERENCES series_trials(request_id) ON DELETE CASCADE,
    episode_id INTEGER NOT NULL REFERENCES episodes(id) ON DELETE CASCADE,
    watched_at TEXT,
    PRIMARY KEY (request_id,episode_id)
) STRICT;
