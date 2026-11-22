package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

// concentrationRetentionDays keeps concentration snapshots a little longer
// than the longest stats window (365 days).
const concentrationRetentionDays = 400

// ReplaceGenerated stores one generation: it replaces the recommendations and
// records impressions like ReplaceOffered and, in the same transaction, the
// top-ten concentration snapshot, so a generation is stored with its snapshot
// or not at all.
func (r *RecommendationRepository) ReplaceGenerated(ctx context.Context, serverID, userID, mediaType string, values []model.Recommendation, c model.RecommendationConcentration) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		if err := replaceRecommendations(ctx, tx, serverID, userID, mediaType, values); err != nil {
			return err
		}
		if err := recordImpressions(ctx, tx, serverID, userID, mediaType, values); err != nil {
			return err
		}
		return recordConcentration(ctx, tx, serverID, userID, c)
	})
}

// recordConcentration stores the top-ten concentration of one generation.
func recordConcentration(ctx context.Context, tx *sql.Tx, serverID, userID string, c model.RecommendationConcentration) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO recommendation_concentration(server_id,user_id,media_type,generated_at,sample_size,dominant_genre,dominant_genre_share,dominant_seed,dominant_seed_share,distinct_interests,distinct_genres)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, serverID, userID, c.MediaType, FormatTime(c.GeneratedAt), c.SampleSize, c.DominantGenre, c.DominantGenreShare, c.DominantSeed, c.DominantSeedShare, c.DistinctInterests, c.DistinctGenres)
	if err != nil {
		return fmt.Errorf("record recommendation concentration: %w", err)
	}
	return nil
}

// concentrationTrendsQuery returns, per media type, the earliest and latest
// snapshot generated at or after ?3 together with the window's averages. ?1 is
// the server, ?2 the user ("" for every user).
const concentrationTrendsQuery = `
WITH recent AS (
    SELECT * FROM recommendation_concentration
    WHERE server_id=?1 AND (?2='' OR user_id=?2) AND generated_at>=?3
), ranked AS (
    SELECT *,
        ROW_NUMBER() OVER (PARTITION BY media_type ORDER BY generated_at, id) AS first_rank,
        ROW_NUMBER() OVER (PARTITION BY media_type ORDER BY generated_at DESC, id DESC) AS last_rank,
        COUNT(*) OVER (PARTITION BY media_type) AS generations,
        AVG(dominant_genre_share) OVER (PARTITION BY media_type) AS average_genre_share,
        AVG(dominant_seed_share) OVER (PARTITION BY media_type) AS average_seed_share,
        AVG(distinct_interests) OVER (PARTITION BY media_type) AS average_interests
    FROM recent
)
SELECT media_type,generated_at,sample_size,dominant_genre,dominant_genre_share,dominant_seed,dominant_seed_share,distinct_interests,distinct_genres,
    first_rank,last_rank,generations,average_genre_share,average_seed_share,average_interests
FROM ranked WHERE first_rank=1 OR last_rank=1
ORDER BY media_type, first_rank`

// ConcentrationTrends summarizes the concentration snapshots generated since
// the given time for one user or, with an empty userID, every user on the
// server, one trend per media type.
func (r *RecommendationRepository) ConcentrationTrends(ctx context.Context, serverID, userID string, since time.Time) ([]model.RecommendationConcentrationTrend, error) {
	var trends = []model.RecommendationConcentrationTrend{}

	rows, err := r.s.ro.QueryContext(ctx, concentrationTrendsQuery, serverID, userID, FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("recommendation concentration: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var snapshot model.RecommendationConcentration
		var trend model.RecommendationConcentrationTrend
		var generated string
		var firstRank, lastRank int

		if err := rows.Scan(&snapshot.MediaType, &generated, &snapshot.SampleSize, &snapshot.DominantGenre, &snapshot.DominantGenreShare,
			&snapshot.DominantSeed, &snapshot.DominantSeedShare, &snapshot.DistinctInterests, &snapshot.DistinctGenres,
			&firstRank, &lastRank, &trend.Generations, &trend.AverageGenreShare, &trend.AverageSeedShare, &trend.AverageDistinctInterests); err != nil {
			return nil, err
		}
		if snapshot.GeneratedAt, err = ParseTime(generated); err != nil {
			return nil, err
		}
		if firstRank == 1 {
			trend.MediaType, trend.Earliest = snapshot.MediaType, snapshot
			trends = append(trends, trend)
		}
		if lastRank == 1 {
			trends[len(trends)-1].Latest = snapshot
		}
	}
	return trends, rows.Err()
}
