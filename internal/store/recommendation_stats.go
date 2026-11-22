package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

// recommendationStatsQuery aggregates the outcome of titles first offered at or
// after ?3. ?1 is the server, ?2 the user ("" for every user). A title counts
// toward a funnel step only when the user acted on it after it was offered, and
// watched only when played after the request.
const recommendationStatsQuery = `
WITH offered AS (
    SELECT user_id,media_type,tmdb_id,first_offered_at FROM recommendation_impressions
    WHERE server_id=?1 AND (?2='' OR user_id=?2) AND first_offered_at>=?3
), requested AS (
    SELECT o.user_id,o.media_type,o.tmdb_id,MIN(f.created_at) AS requested_at
    FROM offered o JOIN recommendation_feedback f ON f.server_id=?1 AND f.user_id=o.user_id AND f.media_type=o.media_type AND f.tmdb_id=o.tmdb_id AND f.action='request' AND f.created_at>=o.first_offered_at
    GROUP BY o.user_id,o.media_type,o.tmdb_id
)
SELECT
    (SELECT MIN(first_offered_at) FROM recommendation_impressions WHERE server_id=?1 AND (?2='' OR user_id=?2)),
    (SELECT COUNT(*) FROM offered),
    (SELECT COUNT(*) FROM requested),
    (SELECT COUNT(*) FROM offered o WHERE EXISTS (SELECT 1 FROM recommendation_feedback f WHERE f.server_id=?1 AND f.user_id=o.user_id AND f.media_type=o.media_type AND f.tmdb_id=o.tmdb_id AND f.action='dismiss' AND f.created_at>=o.first_offered_at)),
    (SELECT COUNT(*) FROM offered o JOIN recommendation_ratings r ON r.server_id=?1 AND r.user_id=o.user_id AND r.media_type=o.media_type AND r.tmdb_id=o.tmdb_id),
    (SELECT COALESCE(AVG(r.rating),0) FROM offered o JOIN recommendation_ratings r ON r.server_id=?1 AND r.user_id=o.user_id AND r.media_type=o.media_type AND r.tmdb_id=o.tmdb_id),
    (SELECT COUNT(*) FROM requested q WHERE EXISTS (SELECT 1 FROM jellyfin_items i WHERE i.server_id=?1 AND i.media_type=q.media_type AND i.tmdb_id=q.tmdb_id AND i.present=1)),
    (SELECT COUNT(*) FROM requested q WHERE EXISTS (SELECT 1 FROM jellyfin_activity a JOIN jellyfin_items i ON i.server_id=a.server_id AND i.item_id=a.item_id
        WHERE a.server_id=?1 AND a.user_id=q.user_id AND a.event_type IN ('played','completed') AND a.occurred_at>=q.requested_at AND i.media_type=q.media_type AND i.tmdb_id=q.tmdb_id)),
    (SELECT COUNT(*) FROM requested q WHERE EXISTS (SELECT 1 FROM jellyfin_activity a JOIN jellyfin_items i ON i.server_id=a.server_id AND i.item_id=a.item_id
        WHERE a.server_id=?1 AND a.user_id=q.user_id AND a.event_type='completed' AND a.occurred_at>=q.requested_at AND i.media_type=q.media_type AND i.tmdb_id=q.tmdb_id))`

// RecommendationStats measures what happened to titles first offered since the
// given time, for one user or, with an empty userID, every user on the server.
// It is computed on demand from the impression and feedback tables.
func (r *RecommendationRepository) RecommendationStats(ctx context.Context, serverID, userID string, since time.Time) (model.RecommendationStats, error) {
	var stats model.RecommendationStats
	var first sql.NullString

	err := r.s.ro.QueryRowContext(ctx, recommendationStatsQuery, serverID, userID, FormatTime(since)).Scan(
		&first, &stats.Offered, &stats.Requested, &stats.Dismissed, &stats.Rated, &stats.AverageRating,
		&stats.Available, &stats.Watched, &stats.Completed)
	if err != nil {
		return stats, fmt.Errorf("recommendation stats: %w", err)
	}
	stats.MeasuringSince, err = scanNullTime(first)
	if err != nil {
		return stats, err
	}
	if stats.Offered > 0 {
		stats.RequestRate = float64(stats.Requested) / float64(stats.Offered)
	}
	return stats, nil
}
