package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/TechXTT/reelay/internal/model"
)

// CountByState counts episodes per state in one query. States without rows are
// absent from the map.
func (r *EpisodeRepository) CountByState(ctx context.Context) (map[model.ItemState]int, error) {
	return countByState[model.ItemState](ctx, r.s.ro, "episodes")
}

// CountByState counts movies per state in one query. States without rows are
// absent from the map.
func (r *MovieRepository) CountByState(ctx context.Context) (map[model.ItemState]int, error) {
	return countByState[model.ItemState](ctx, r.s.ro, "movies")
}

// CountByState counts grabs per state in one query. States without rows are
// absent from the map.
func (r *GrabRepository) CountByState(ctx context.Context) (map[model.GrabState]int, error) {
	return countByState[model.GrabState](ctx, r.s.ro, "grabs")
}

// PendingNotifications counts availability events still waiting for delivery,
// using the same filter as ClaimNotification but ignoring the retry time.
func (r *RequestRepository) PendingNotifications(ctx context.Context) (int, error) {
	var count int
	var err = r.s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM availability_outbox o JOIN media_requests mr ON mr.id=o.request_id WHERE o.delivered_at IS NULL AND mr.cancelled_at IS NULL`).Scan(&count)

	if err != nil {
		return 0, fmt.Errorf("count pending notifications: %w", err)
	}
	return count, nil
}

// countByState runs a GROUP BY over a state column. table is always a
// package-internal constant, never caller input.
func countByState[S ~string](ctx context.Context, db *sql.DB, table string) (map[S]int, error) {
	var rows, err = db.QueryContext(ctx, "SELECT state, COUNT(*) FROM "+table+" GROUP BY state")
	var counts = make(map[S]int)

	if err != nil {
		return nil, fmt.Errorf("count %s by state: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var state S
		var count int

		if err := rows.Scan(&state, &count); err != nil {
			return nil, fmt.Errorf("count %s by state: %w", table, err)
		}
		counts[state] = count
	}
	return counts, rows.Err()
}
