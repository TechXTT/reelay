package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/TechXTT/reelay/internal/model"
)

func recommendationFeatures(value model.Recommendation, directionPick bool) string {
	features, _ := encodeJSON(map[string]any{"components": value.Components, "genres": value.Genres, "keywords": value.Keywords, "people": value.People, "language": value.Language, "country": value.Country, "runtime_minutes": value.RuntimeMinutes, "vote_average": value.VoteAverage, "vote_count": value.VoteCount, "direction_pick": directionPick})
	return features
}

// OfferDirection gives temporary picks stable IDs for the regular preview and
// request actions. Background generation can promote them into the regular feed.
func (r *RecommendationRepository) OfferDirection(ctx context.Context, serverID, userID, mediaType string, picks []model.Recommendation) ([]model.Recommendation, error) {
	var offered = make([]model.Recommendation, 0, len(picks))
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		for _, pick := range picks {
			reasons, _ := encodeJSON(pick.Reasons)
			_, err := tx.ExecContext(ctx, `INSERT INTO recommendations(server_id,user_id,media_type,tmdb_id,title,year,overview,poster_url,score,reasons_json,features_json,status,generated_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,'active',?,?) ON CONFLICT(server_id,user_id,media_type,tmdb_id) DO UPDATE SET expires_at=MAX(recommendations.expires_at,excluded.expires_at)
WHERE recommendations.status='active' AND json_extract(recommendations.features_json,'$.direction_pick')=1`,
				serverID, userID, mediaType, pick.TMDBID, pick.Title, pick.Year, pick.Overview, pick.PosterURL, pick.Score, reasons, recommendationFeatures(pick, true), FormatTime(pick.GeneratedAt), FormatTime(pick.ExpiresAt))
			if err != nil {
				return err
			}
			err = tx.QueryRowContext(ctx, `SELECT id FROM recommendations WHERE server_id=? AND user_id=? AND media_type=? AND tmdb_id=? AND status='active'`, serverID, userID, mediaType, pick.TMDBID).Scan(&pick.ID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			offered = append(offered, pick)
		}
		return recordImpressions(ctx, tx, serverID, userID, mediaType, offered)
	})
	return offered, err
}
