-- Later confirmed episodes of a request get their own availability events.
-- episode_notifications records every episode an event has covered, so an
-- episode is announced at most once, even after a withdrawn confirmation is
-- confirmed again. The outboxes become unique per (request, item) so a request
-- can hold one event per batch; title-level events keep one per request in code.
CREATE TABLE episode_notifications (
    request_id  INTEGER NOT NULL REFERENCES media_requests(id) ON DELETE CASCADE,
    season      INTEGER NOT NULL CHECK (season >= 0),
    number      INTEGER NOT NULL CHECK (number > 0),
    notified_at TEXT NOT NULL,
    PRIMARY KEY (request_id, season, number)
) STRICT;

-- Episodes already confirmed for a request that has an event were covered by it.
INSERT INTO episode_notifications(request_id,season,number,notified_at)
SELECT c.request_id,c.season,c.number,c.confirmed_at FROM jellyfin_episode_confirmations c
WHERE EXISTS(SELECT 1 FROM availability_outbox o WHERE o.request_id=c.request_id);

CREATE TABLE availability_outbox_new (
    id INTEGER PRIMARY KEY,
    request_id INTEGER NOT NULL REFERENCES media_requests(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL,
    next_attempt_at TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    delivered_at TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    UNIQUE (request_id, item_id)
) STRICT;
INSERT INTO availability_outbox_new SELECT id,request_id,item_id,payload,created_at,next_attempt_at,attempts,delivered_at,last_error FROM availability_outbox;
DROP TABLE availability_outbox;
ALTER TABLE availability_outbox_new RENAME TO availability_outbox;
CREATE INDEX idx_availability_outbox_due ON availability_outbox(next_attempt_at) WHERE delivered_at IS NULL;

CREATE TABLE user_availability_outbox_new (
    id INTEGER PRIMARY KEY,
    request_id INTEGER NOT NULL REFERENCES media_requests(id) ON DELETE CASCADE,
    item_id TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at TEXT NOT NULL,
    next_attempt_at TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    delivered_at TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    UNIQUE (request_id, item_id)
) STRICT;
INSERT INTO user_availability_outbox_new SELECT id,request_id,item_id,payload,created_at,next_attempt_at,attempts,delivered_at,last_error FROM user_availability_outbox;
DROP TABLE user_availability_outbox;
ALTER TABLE user_availability_outbox_new RENAME TO user_availability_outbox;
CREATE INDEX idx_user_availability_outbox_due ON user_availability_outbox(next_attempt_at) WHERE delivered_at IS NULL;

-- Current played state from the plugin's per-user library pass. played_reported
-- is 1 when the pass carried played state; otherwise Tonight falls back to
-- watch activity, which cannot tell a title later marked unplayed.
ALTER TABLE jellyfin_user_items ADD COLUMN played INTEGER NOT NULL DEFAULT 0 CHECK (played IN (0, 1));
ALTER TABLE jellyfin_user_items ADD COLUMN played_reported INTEGER NOT NULL DEFAULT 0 CHECK (played_reported IN (0, 1));
