-- Per-user Jellyfin library visibility and Save for later.
--
-- The plugin reports, per enabled user, which catalog items that user can
-- access. Each pass stages rows under its own sync token; completing the pass
-- points jellyfin_user_libraries at that token and deletes every other token's
-- rows in one transaction, so readers see either the previous or the new set.
-- A user without a jellyfin_user_libraries row has no per-user data yet (an
-- older plugin), which is different from an empty library.
CREATE TABLE jellyfin_user_libraries (
    server_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    sync_token TEXT NOT NULL,
    synced_at  TEXT NOT NULL,
    PRIMARY KEY (server_id, user_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE jellyfin_user_items (
    server_id  TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    sync_token TEXT NOT NULL,
    item_id    TEXT NOT NULL,
    PRIMARY KEY (server_id, user_id, sync_token, item_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE,
    FOREIGN KEY (server_id, item_id) REFERENCES jellyfin_items(server_id, item_id) ON DELETE CASCADE
) STRICT;

CREATE TABLE saved_for_later (
    server_id TEXT NOT NULL,
    user_id   TEXT NOT NULL,
    item_id   TEXT NOT NULL,
    saved_at  TEXT NOT NULL,
    PRIMARY KEY (server_id, user_id, item_id),
    FOREIGN KEY (server_id, user_id) REFERENCES jellyfin_users(server_id, user_id) ON DELETE CASCADE,
    FOREIGN KEY (server_id, item_id) REFERENCES jellyfin_items(server_id, item_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_saved_for_later_user ON saved_for_later (server_id, user_id, saved_at DESC);
