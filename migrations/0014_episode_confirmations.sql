-- Jellyfin's confirmation that a requested episode is visible to the requester.
-- The plugin resolves each candidate under the series with that user's access
-- and reports the episode item. The first confirmation of a request queues its
-- availability notification; a series parent alone never does once the plugin
-- reports this capability.
CREATE TABLE jellyfin_episode_confirmations (
    request_id   INTEGER NOT NULL REFERENCES media_requests(id) ON DELETE CASCADE,
    season       INTEGER NOT NULL CHECK (season >= 0),
    number       INTEGER NOT NULL CHECK (number > 0),
    item_id      TEXT NOT NULL CHECK (item_id <> ''),
    confirmed_at TEXT NOT NULL,
    PRIMARY KEY (request_id, season, number)
) STRICT;
