package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SavedForLaterLimit caps how many titles one user can keep in Save for later.
const SavedForLaterLimit = 200

// ErrSavedForLaterFull means the user already has SavedForLaterLimit titles saved.
var ErrSavedForLaterFull = errors.New("save for later is full")

// TonightItem is a title from one Jellyfin user's libraries. Playable is false
// for a saved title the user can no longer access; the API then omits links.
type TonightItem struct {
	ServerID       string     `json:"server_id"`
	ItemID         string     `json:"item_id"`
	MediaType      string     `json:"media_type"`
	Title          string     `json:"title"`
	Year           int        `json:"year,omitempty"`
	Genres         []string   `json:"genres"`
	RuntimeMinutes int        `json:"runtime_minutes,omitempty"`
	Completed      bool       `json:"completed"`
	SavedAt        *time.Time `json:"saved_at,omitempty"`
	Playable       bool       `json:"playable"`
	PosterURL      string     `json:"poster_url,omitempty"`
	JellyfinURL    string     `json:"jellyfin_url,omitempty"`
}

// TonightFilter narrows one user's playable titles for a single request.
// MediaType "" means both, MaxRuntime 0 means no limit.
type TonightFilter struct {
	ServerID   string
	UserID     string
	MediaType  string
	MaxRuntime int
	Unfinished bool
	Limit      int
	Offset     int
}

type TonightRepository struct{ s *Store }

func (s *Store) Tonight() *TonightRepository { return &TonightRepository{s: s} }

// visibleItemSQL and completedSQL test one user's item. %[1]s names a row
// source with server_id, user_id and item_id columns. An item is visible when
// it is in the user's last completed library pass.
const visibleItemSQL = `EXISTS(SELECT 1 FROM jellyfin_user_libraries l JOIN jellyfin_user_items v
 ON v.server_id=l.server_id AND v.user_id=l.user_id AND v.sync_token=l.sync_token
 WHERE l.server_id=%[1]s.server_id AND l.user_id=%[1]s.user_id AND v.item_id=%[1]s.item_id)`

const completedSQL = `EXISTS(SELECT 1 FROM jellyfin_activity a WHERE a.server_id=%[1]s.server_id AND a.user_id=%[1]s.user_id
 AND a.item_id=%[1]s.item_id AND a.event_type='completed')`

func userExists(ctx context.Context, tx *sql.Tx, serverID, userID string) error {
	var exists bool

	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jellyfin_users WHERE server_id=? AND user_id=?)`, serverID, userID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check jellyfin user: %w", err)
	}
	if !exists {
		return fmt.Errorf("jellyfin user %s: %w", userID, ErrNotFound)
	}
	return nil
}

// StageUserItems records catalog items the user can access under syncToken.
// Items the catalog does not know are skipped. Staged rows stay invisible
// until CompleteUserItems makes syncToken current. It returns how many new
// rows were staged.
func (r *TonightRepository) StageUserItems(ctx context.Context, serverID, userID, syncToken string, itemIDs []string) (int, error) {
	var staged int64

	if len(itemIDs) == 0 {
		return 0, nil
	}
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		if err := userExists(ctx, tx, serverID, userID); err != nil {
			return err
		}
		for _, itemID := range itemIDs {
			result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jellyfin_user_items(server_id,user_id,sync_token,item_id)
SELECT server_id,?,?,item_id FROM jellyfin_items WHERE server_id=? AND item_id=?`, userID, syncToken, serverID, itemID)
			if err != nil {
				return fmt.Errorf("stage jellyfin user item %s: %w", itemID, err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			staged += count
		}
		return nil
	})
	return int(staged), err
}

// CompleteUserItems makes syncToken the user's visible library and discards
// every other pass in the same transaction. It returns the visible item count.
func (r *TonightRepository) CompleteUserItems(ctx context.Context, serverID, userID, syncToken string) (int, error) {
	var visible int

	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = FormatTime(r.s.nowUTC())

		if err := userExists(ctx, tx, serverID, userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO jellyfin_user_libraries(server_id,user_id,sync_token,synced_at) VALUES(?,?,?,?)
ON CONFLICT(server_id,user_id) DO UPDATE SET sync_token=excluded.sync_token,synced_at=excluded.synced_at`, serverID, userID, syncToken, now)
		if err != nil {
			return fmt.Errorf("complete jellyfin user library: %w", err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM jellyfin_user_items WHERE server_id=? AND user_id=? AND sync_token<>?`, serverID, userID, syncToken)
		if err != nil {
			return fmt.Errorf("discard previous jellyfin user library: %w", err)
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jellyfin_user_items WHERE server_id=? AND user_id=? AND sync_token=?`, serverID, userID, syncToken).Scan(&visible)
	})
	return visible, err
}

// LibrarySyncedAt is when the user's library visibility last completed, or nil
// when the plugin has never reported it (an older plugin).
func (r *TonightRepository) LibrarySyncedAt(ctx context.Context, serverID, userID string) (*time.Time, error) {
	var synced sql.NullString

	err := r.s.ro.QueryRowContext(ctx, `SELECT synced_at FROM jellyfin_user_libraries WHERE server_id=? AND user_id=?`, serverID, userID).Scan(&synced)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read jellyfin user library: %w", err)
	}
	return scanNullTime(synced)
}

func scanTonightItem(row scanner) (TonightItem, error) {
	var value TonightItem
	var genres string
	var saved sql.NullString

	err := row.Scan(&value.ServerID, &value.ItemID, &value.MediaType, &value.Title, &value.Year, &genres, &value.RuntimeMinutes, &value.Completed, &saved, &value.Playable)
	if err != nil {
		return value, err
	}
	if err := decodeJSON(genres, &value.Genres); err != nil {
		return value, err
	}
	value.SavedAt, err = scanNullTime(saved)
	return value, err
}

// List returns one page of the user's playable movies and series ordered by
// title, and whether more follow. Titles without a known runtime are left out
// while a runtime limit applies.
func (r *TonightRepository) List(ctx context.Context, filter TonightFilter) ([]TonightItem, bool, error) {
	rows, err := r.s.ro.QueryContext(ctx, `WITH visible AS (
 SELECT i.server_id,i.item_id,i.media_type,i.title,i.year,i.genres_json,i.runtime_minutes,
  `+fmt.Sprintf(completedSQL, "v")+` AS completed,s.saved_at
 FROM jellyfin_user_libraries l
 JOIN jellyfin_user_items v ON v.server_id=l.server_id AND v.user_id=l.user_id AND v.sync_token=l.sync_token
 JOIN jellyfin_items i ON i.server_id=v.server_id AND i.item_id=v.item_id AND i.present=1
 LEFT JOIN saved_for_later s ON s.server_id=v.server_id AND s.user_id=v.user_id AND s.item_id=v.item_id
 WHERE l.server_id=? AND l.user_id=?
)
SELECT server_id,item_id,media_type,title,year,genres_json,runtime_minutes,completed,saved_at,1 FROM visible
WHERE (?='' OR media_type=?) AND (?=0 OR runtime_minutes BETWEEN 1 AND ?) AND (?=0 OR completed=0)
ORDER BY title COLLATE NOCASE,item_id LIMIT ? OFFSET ?`,
		filter.ServerID, filter.UserID, filter.MediaType, filter.MediaType, filter.MaxRuntime, filter.MaxRuntime, filter.Unfinished, filter.Limit+1, filter.Offset)
	if err != nil {
		return nil, false, fmt.Errorf("list tonight items: %w", err)
	}
	values, err := collectRows(rows, scanTonightItem)
	if err != nil {
		return nil, false, fmt.Errorf("list tonight items: %w", err)
	}
	if len(values) > filter.Limit {
		return values[:filter.Limit], true, nil
	}
	return values, false, nil
}

// Saved returns the user's saved titles, newest first. A title the user can no
// longer access, or that left the catalog, is returned with Playable false.
func (r *TonightRepository) Saved(ctx context.Context, serverID, userID string) ([]TonightItem, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT s.server_id,s.item_id,i.media_type,i.title,i.year,i.genres_json,i.runtime_minutes,
 `+fmt.Sprintf(completedSQL, "s")+`,s.saved_at,i.present=1 AND `+fmt.Sprintf(visibleItemSQL, "s")+`
FROM saved_for_later s JOIN jellyfin_items i ON i.server_id=s.server_id AND i.item_id=s.item_id
WHERE s.server_id=? AND s.user_id=? ORDER BY s.saved_at DESC,s.item_id LIMIT ?`, serverID, userID, SavedForLaterLimit)
	if err != nil {
		return nil, fmt.Errorf("list saved for later: %w", err)
	}
	values, err := collectRows(rows, scanTonightItem)
	if err != nil {
		return nil, fmt.Errorf("list saved for later: %w", err)
	}
	return values, nil
}

// Save keeps a title the user can currently access. It returns ErrNotFound for
// any other title and ErrSavedForLaterFull at the limit; saving again is a no-op
// that reports false.
func (r *TonightRepository) Save(ctx context.Context, serverID, userID, itemID string) (bool, error) {
	var created bool

	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var visible bool
		var saved bool
		var count int

		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jellyfin_items i WHERE i.server_id=t.server_id AND i.item_id=t.item_id AND i.present=1)
 AND `+fmt.Sprintf(visibleItemSQL, "t")+`,
 EXISTS(SELECT 1 FROM saved_for_later x WHERE x.server_id=t.server_id AND x.user_id=t.user_id AND x.item_id=t.item_id),
 (SELECT COUNT(*) FROM saved_for_later x WHERE x.server_id=t.server_id AND x.user_id=t.user_id)
FROM (SELECT ? AS server_id,? AS user_id,? AS item_id) t`, serverID, userID, itemID).Scan(&visible, &saved, &count)
		if err != nil {
			return fmt.Errorf("check save for later: %w", err)
		}
		if !visible {
			return fmt.Errorf("jellyfin item %s: %w", itemID, ErrNotFound)
		}
		if saved {
			return nil
		}
		if count >= SavedForLaterLimit {
			return ErrSavedForLaterFull
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO saved_for_later(server_id,user_id,item_id,saved_at) VALUES(?,?,?,?)`, serverID, userID, itemID, FormatTime(r.s.nowUTC()))
		if err != nil {
			return fmt.Errorf("save for later: %w", err)
		}
		created = true
		return nil
	})
	return created, err
}

// Unsave removes a saved title and reports whether one was removed.
func (r *TonightRepository) Unsave(ctx context.Context, serverID, userID, itemID string) (bool, error) {
	result, err := r.s.rw.ExecContext(ctx, `DELETE FROM saved_for_later WHERE server_id=? AND user_id=? AND item_id=?`, serverID, userID, itemID)
	if err != nil {
		return false, fmt.Errorf("remove saved for later: %w", err)
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
