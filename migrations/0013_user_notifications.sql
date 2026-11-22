-- Per-user availability notifications. A member chooses where their own
-- "ready to watch" messages go; the destination is separate from the global
-- availability webhook so one household member cannot redirect another's.
CREATE TABLE notification_destinations (
    server_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('ntfy', 'webhook')),
    topic      TEXT NOT NULL DEFAULT '',
    url        TEXT NOT NULL DEFAULT '',
    format     TEXT NOT NULL DEFAULT 'json' CHECK (format IN ('json', 'ntfy')),
    enabled    INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (server_id, user_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

-- Same shape and retry columns as availability_outbox.
CREATE TABLE user_availability_outbox (
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
CREATE INDEX idx_user_availability_outbox_due ON user_availability_outbox(next_attempt_at) WHERE delivered_at IS NULL;
