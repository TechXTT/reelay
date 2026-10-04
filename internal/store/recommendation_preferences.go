package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

type RecommendationPreferences struct {
	Languages      []string `json:"languages"`
	ExcludedGenres []string `json:"excluded_genres"`
	Familiarity    string   `json:"familiarity"`
	Diversity      int      `json:"diversity"`
}

func (r *RecommendationRepository) Preferences(ctx context.Context, serverID, userID string) (RecommendationPreferences, error) {
	var preferences = RecommendationPreferences{Languages: []string{}, ExcludedGenres: []string{}, Familiarity: "balanced", Diversity: 100}
	var identity, _ = json.Marshal([]string{serverID, userID})
	var raw, err = r.s.KV().Get(ctx, "recommendation-preferences:"+string(identity))

	if errors.Is(err, ErrNotFound) {
		return preferences, nil
	}
	if err != nil {
		return preferences, err
	}
	return preferences, json.Unmarshal([]byte(raw), &preferences)
}

func (r *RecommendationRepository) SavePreferences(ctx context.Context, serverID, userID string, preferences RecommendationPreferences) error {
	var identity, _ = json.Marshal([]string{serverID, userID})
	var raw, err = json.Marshal(preferences)

	if err != nil {
		return err
	}
	if serverID == "" || userID == "" {
		return errors.New("server_id and user_id are required")
	}
	if len(preferences.Languages) > 20 || len(preferences.ExcludedGenres) > 30 || preferences.Diversity < 0 || preferences.Diversity > 100 {
		return errors.New("preferences exceed bounds")
	}
	if preferences.Familiarity != "balanced" && preferences.Familiarity != "familiar" && preferences.Familiarity != "explore" {
		return errors.New("familiarity must be balanced, familiar, or explore")
	}
	for i, language := range preferences.Languages {
		language = strings.ToLower(strings.TrimSpace(language))
		if len(language) != 2 || language[0] < 'a' || language[0] > 'z' || language[1] < 'a' || language[1] > 'z' {
			return errors.New("languages must be two-letter language codes")
		}
		preferences.Languages[i] = language
	}
	for i, genre := range preferences.ExcludedGenres {
		genre = strings.TrimSpace(genre)
		if genre == "" || len(genre) > 64 {
			return errors.New("genre names must contain 1 to 64 characters")
		}
		preferences.ExcludedGenres[i] = genre
	}
	raw, err = json.Marshal(preferences)
	if err != nil {
		return err
	}
	return r.s.KV().Set(ctx, "recommendation-preferences:"+string(identity), string(raw))
}

func (r *RecommendationRepository) UndoDismissal(ctx context.Context, id int64) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var value, err = r.Get(ctx, id)
		var rated int

		if err != nil {
			return err
		}
		if value.Status != "dismissed" {
			return errors.New("only a dismissed recommendation can be restored")
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM recommendation_ratings WHERE server_id=? AND user_id=? AND media_type=? AND tmdb_id=?`, value.ServerID, value.UserID, value.MediaType, value.TMDBID).Scan(&rated); err != nil {
			return err
		}
		if rated > 0 {
			return errors.New("rated titles remain excluded; edit the rating instead")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recommendation_feedback WHERE server_id=? AND user_id=? AND media_type=? AND tmdb_id=? AND action='dismiss'`, value.ServerID, value.UserID, value.MediaType, value.TMDBID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE recommendations SET status='active' WHERE id=?`, id)
		return err
	})
}

func (r *RecommendationRepository) HistoryRatings(ctx context.Context, serverID, userID, mediaType string) ([]model.RecommendationRating, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT server_id,user_id,media_type,tmdb_id,rating,updated_at FROM recommendation_ratings
WHERE server_id=? AND user_id=? AND media_type=? AND tmdb_id IN
(SELECT tmdb_id FROM recommendations WHERE server_id=? AND user_id=? AND media_type=? AND status='dismissed' ORDER BY score DESC,id LIMIT 100)`, serverID, userID, mediaType, serverID, userID, mediaType)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(row scanner) (model.RecommendationRating, error) {
		var value model.RecommendationRating
		var updated string
		if err := row.Scan(&value.ServerID, &value.UserID, &value.MediaType, &value.TMDBID, &value.Rating, &updated); err != nil {
			return value, err
		}
		var err error
		value.UpdatedAt, err = ParseTime(updated)
		return value, err
	})
}
