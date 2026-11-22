package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type searchHold struct {
	Since   *time.Time    `json:"since,omitempty"`
	Elapsed time.Duration `json:"elapsed_ns"`
}

// updateSearchHold changes budget bookkeeping in the same transaction as the
// search outcome. The wanted timestamp distinguishes reused item IDs.
func updateSearchHold(ctx context.Context, tx *sql.Tx, lock *ItemLock, firstWanted, now time.Time,
	holding bool) (time.Duration, error) {
	var hold searchHold
	var raw string

	key := fmt.Sprintf("search.hold.%s.%d.%s", lock.Subject, lock.ID, FormatTime(firstWanted))
	err := tx.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", key).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, err
	default:
		if err := json.Unmarshal([]byte(raw), &hold); err != nil {
			return 0, fmt.Errorf("decode search hold %q: %w", key, err)
		}
	}
	if holding == (hold.Since != nil) {
		return hold.Elapsed, nil
	}
	if holding {
		hold.Since = &now
	} else {
		hold.Elapsed += max(0, now.Sub(*hold.Since))
		hold.Since = nil
	}
	body, err := json.Marshal(hold)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
 ON CONFLICT (key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, string(body), FormatTime(now))
	return hold.Elapsed, err
}
