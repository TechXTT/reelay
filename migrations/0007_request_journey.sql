ALTER TABLE media_requests ADD COLUMN cancelled_at TEXT;
ALTER TABLE media_requests ADD COLUMN seasons_json TEXT NOT NULL DEFAULT '[]';

CREATE TABLE series_seasons (
    series_id INTEGER NOT NULL REFERENCES series(id) ON DELETE CASCADE,
    season INTEGER NOT NULL CHECK (season BETWEEN 0 AND 999),
    PRIMARY KEY (series_id, season)
) STRICT;

CREATE TABLE availability_outbox (
    id INTEGER PRIMARY KEY,
    request_id INTEGER NOT NULL REFERENCES media_requests(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL,
    next_attempt_at TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    delivered_at TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    UNIQUE (request_id)
) STRICT;
CREATE INDEX idx_availability_outbox_due ON availability_outbox(next_attempt_at) WHERE delivered_at IS NULL;
