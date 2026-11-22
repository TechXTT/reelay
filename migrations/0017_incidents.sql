-- Operator incidents group the items that fail or wait for one underlying
-- cause, such as an indexer outage or a full volume, under a single record.
-- cause_key identifies the cause within its kind (an indexer name, a root
-- path). detail is the raw error and is only ever shown to operators.
CREATE TABLE incidents (
    id            INTEGER PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('indexer', 'download_client', 'storage', 'library', 'metadata', 'notifications')),
    cause_key     TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT '',
    affected      INTEGER NOT NULL DEFAULT 0 CHECK (affected >= 0),
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    next_check_at TEXT,
    resolved_at   TEXT
) STRICT;

-- At most one open incident per cause.
CREATE UNIQUE INDEX idx_incidents_open ON incidents (kind, cause_key) WHERE resolved_at IS NULL;
CREATE INDEX idx_incidents_resolved ON incidents (resolved_at DESC) WHERE resolved_at IS NOT NULL;
