package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

type RecommendationRepository struct{ s *Store }

func (s *Store) Recommendations() *RecommendationRepository { return &RecommendationRepository{s: s} }

func (r *RecommendationRepository) UpsertUser(ctx context.Context, user model.JellyfinUser) error {
	if strings.TrimSpace(user.ServerID) == "" || strings.TrimSpace(user.UserID) == "" || strings.TrimSpace(user.DisplayName) == "" {
		return errors.New("jellyfin user requires server_id, user_id and display_name")
	}
	now := r.s.nowUTC()
	_, err := r.s.rw.ExecContext(ctx, `INSERT INTO jellyfin_users(server_id,user_id,display_name,enabled,last_synced_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(server_id,user_id) DO UPDATE SET display_name=excluded.display_name, enabled=excluded.enabled, last_synced_at=excluded.last_synced_at, updated_at=excluded.updated_at`,
		user.ServerID, user.UserID, user.DisplayName, user.Enabled, nullTime(&user.LastSynced), FormatTime(now), FormatTime(now))
	if err != nil {
		return fmt.Errorf("upsert jellyfin user: %w", err)
	}
	return nil
}

func (r *RecommendationRepository) Users(ctx context.Context) ([]model.JellyfinUser, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT server_id,user_id,display_name,enabled,last_synced_at FROM jellyfin_users ORDER BY display_name`)
	if err != nil {
		return nil, fmt.Errorf("list jellyfin users: %w", err)
	}
	return collectRows(rows, func(row scanner) (model.JellyfinUser, error) {
		var value model.JellyfinUser
		var enabled int
		var synced sql.NullString
		if err := row.Scan(&value.ServerID, &value.UserID, &value.DisplayName, &enabled, &synced); err != nil {
			return value, err
		}
		value.Enabled = enabled == 1
		if t, err := scanNullTime(synced); err != nil {
			return value, err
		} else if t != nil {
			value.LastSynced = *t
		}
		return value, nil
	})
}

func (r *RecommendationRepository) UpsertItems(ctx context.Context, items []model.JellyfinItem, syncToken string) error {
	if len(items) == 0 {
		return nil
	}
	if strings.TrimSpace(syncToken) == "" {
		return errors.New("jellyfin item sync requires a sync token")
	}
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		now := FormatTime(r.s.nowUTC())
		for _, item := range items {
			if item.ServerID == "" || item.ItemID == "" || (item.MediaType != "movie" && item.MediaType != "series") || item.Title == "" {
				return errors.New("invalid jellyfin item")
			}
			genres, _ := encodeJSON(item.Genres)
			keywords, _ := encodeJSON(item.Keywords)
			people, _ := encodeJSON(item.People)
			_, err := tx.ExecContext(ctx, `INSERT INTO jellyfin_items(server_id,item_id,media_type,tmdb_id,tvdb_id,imdb_id,title,year,genres_json,keywords_json,people_json,language,country,runtime_minutes,present,sync_token,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,item_id) DO UPDATE SET media_type=excluded.media_type,tmdb_id=excluded.tmdb_id,tvdb_id=excluded.tvdb_id,imdb_id=excluded.imdb_id,title=excluded.title,year=excluded.year,genres_json=excluded.genres_json,keywords_json=excluded.keywords_json,people_json=excluded.people_json,language=excluded.language,country=excluded.country,runtime_minutes=excluded.runtime_minutes,present=excluded.present,sync_token=excluded.sync_token,updated_at=excluded.updated_at`,
				item.ServerID, item.ItemID, item.MediaType, item.TMDBID, item.TVDBID, item.IMDBID, item.Title, item.Year, genres, keywords, people, item.Language, item.Country, item.RuntimeMinutes, item.Present, syncToken, now)
			if err != nil {
				return fmt.Errorf("upsert jellyfin item %s: %w", item.ItemID, err)
			}
		}
		return nil
	})
}

func (r *RecommendationRepository) CompleteSync(ctx context.Context, serverID, syncToken string) (int, error) {
	if strings.TrimSpace(serverID) == "" || strings.TrimSpace(syncToken) == "" {
		return 0, errors.New("complete jellyfin sync requires server id and sync token")
	}
	result, err := r.s.rw.ExecContext(ctx, `UPDATE jellyfin_items SET present=0,updated_at=? WHERE server_id=? AND sync_token<>? AND present=1`, FormatTime(r.s.nowUTC()), serverID, syncToken)
	if err != nil {
		return 0, fmt.Errorf("complete jellyfin item sync: %w", err)
	}
	count, err := result.RowsAffected()
	return int(count), err
}

func (r *RecommendationRepository) AddActivities(ctx context.Context, events []model.JellyfinActivity) (int, error) {
	inserted := 0
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		for _, event := range events {
			if event.EventID == "" || event.ServerID == "" || event.UserID == "" || event.ItemID == "" || event.OccurredAt.IsZero() {
				return errors.New("invalid jellyfin activity")
			}
			res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO jellyfin_activity(event_id,server_id,user_id,item_id,event_type,progress,occurred_at) VALUES(?,?,?,?,?,?,?)`, event.EventID, event.ServerID, event.UserID, event.ItemID, event.EventType, event.Progress, FormatTime(event.OccurredAt))
			if err != nil {
				return fmt.Errorf("insert jellyfin activity: %w", err)
			}
			n, _ := res.RowsAffected()
			inserted += int(n)
		}
		return nil
	})
	return inserted, err
}

func (r *RecommendationRepository) PositiveSeeds(ctx context.Context, serverID, userID, mediaType string, limit int) ([]model.JellyfinItem, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	rows, err := r.s.ro.QueryContext(ctx, `SELECT i.server_id,i.item_id,i.media_type,i.tmdb_id,i.tvdb_id,i.imdb_id,i.title,i.year,i.genres_json,i.keywords_json,i.people_json,i.language,i.country,i.runtime_minutes,i.present
FROM jellyfin_items i JOIN jellyfin_activity a ON a.server_id=i.server_id AND a.item_id=i.item_id
WHERE a.server_id=? AND a.user_id=? AND i.media_type=? AND i.present=1 AND i.tmdb_id>0 AND a.event_type IN ('favorite','like','completed')
GROUP BY i.server_id,i.item_id ORDER BY MAX(a.occurred_at) DESC LIMIT ?`, serverID, userID, mediaType, limit)
	if err != nil {
		return nil, fmt.Errorf("list recommendation seeds: %w", err)
	}
	return collectRows(rows, scanJellyfinItem)
}

func (r *RecommendationRepository) OwnedTMDBIDs(ctx context.Context, serverID, mediaType string) (map[int]bool, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT tmdb_id FROM jellyfin_items WHERE server_id=? AND media_type=? AND present=1 AND tmdb_id>0`, serverID, mediaType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (r *RecommendationRepository) Replace(ctx context.Context, serverID, userID, mediaType string, values []model.Recommendation) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM recommendations WHERE server_id=? AND user_id=? AND media_type=? AND status='active'`, serverID, userID, mediaType); err != nil {
			return err
		}
		for _, value := range values {
			reasons, _ := encodeJSON(value.Reasons)
			features, _ := encodeJSON(map[string]any{"components": value.Components, "genres": value.Genres, "keywords": value.Keywords, "people": value.People, "language": value.Language, "country": value.Country, "runtime_minutes": value.RuntimeMinutes})
			_, err := tx.ExecContext(ctx, `INSERT INTO recommendations(server_id,user_id,media_type,tmdb_id,title,year,overview,poster_url,score,reasons_json,features_json,status,generated_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,user_id,media_type,tmdb_id) DO UPDATE SET title=excluded.title,year=excluded.year,overview=excluded.overview,poster_url=excluded.poster_url,score=excluded.score,reasons_json=excluded.reasons_json,features_json=excluded.features_json,generated_at=excluded.generated_at,expires_at=excluded.expires_at,status=CASE WHEN recommendations.status IN ('dismissed','requested','available') THEN recommendations.status ELSE 'active' END`,
				serverID, userID, mediaType, value.TMDBID, value.Title, value.Year, value.Overview, value.PosterURL, value.Score, reasons, features, "active", FormatTime(value.GeneratedAt), FormatTime(value.ExpiresAt))
			if err != nil {
				return fmt.Errorf("store recommendation %d: %w", value.TMDBID, err)
			}
		}
		return nil
	})
}

func (r *RecommendationRepository) List(ctx context.Context, serverID, userID, mediaType, status string, limit, offset int) ([]model.Recommendation, error) {
	if limit <= 0 || limit > 100 {
		limit = 40
	}
	if offset < 0 {
		offset = 0
	}
	if status == "" {
		status = "active"
	}
	rows, err := r.s.ro.QueryContext(ctx, `SELECT id,server_id,user_id,media_type,tmdb_id,title,year,overview,poster_url,score,reasons_json,features_json,status,generated_at,expires_at FROM recommendations WHERE server_id=? AND user_id=? AND (?='' OR media_type=?) AND status=? ORDER BY score DESC,id LIMIT ? OFFSET ?`, serverID, userID, mediaType, mediaType, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list recommendations: %w", err)
	}
	return collectRows(rows, scanRecommendation)
}

func (r *RecommendationRepository) Get(ctx context.Context, id int64) (model.Recommendation, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, `SELECT id,server_id,user_id,media_type,tmdb_id,title,year,overview,poster_url,score,reasons_json,features_json,status,generated_at,expires_at FROM recommendations WHERE id=?`, id), scanRecommendation, fmt.Sprintf("recommendation %d", id))
}

func (r *RecommendationRepository) RecordAction(ctx context.Context, id int64, actionID, action string) (model.Recommendation, bool, error) {
	if action != "dismiss" && action != "request" {
		return model.Recommendation{}, false, fmt.Errorf("invalid recommendation action %q", action)
	}
	v, err := r.Get(ctx, id)
	if err != nil {
		return v, false, err
	}
	inserted := false
	err = r.s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO recommendation_feedback(action_id,server_id,user_id,media_type,tmdb_id,action,created_at) VALUES(?,?,?,?,?,?,?)`, actionID, v.ServerID, v.UserID, v.MediaType, v.TMDBID, action, FormatTime(r.s.nowUTC()))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		inserted = n == 1
		if inserted {
			status := "dismissed"
			if action == "request" {
				status = "requested"
			}
			_, err = tx.ExecContext(ctx, `UPDATE recommendations SET status=? WHERE id=?`, status, id)
		}
		return err
	})
	return v, inserted, err
}

func scanJellyfinItem(row scanner) (model.JellyfinItem, error) {
	var v model.JellyfinItem
	var genres, keywords, people string
	var present int
	err := row.Scan(&v.ServerID, &v.ItemID, &v.MediaType, &v.TMDBID, &v.TVDBID, &v.IMDBID, &v.Title, &v.Year, &genres, &keywords, &people, &v.Language, &v.Country, &v.RuntimeMinutes, &present)
	if err != nil {
		return v, err
	}
	v.Present = present == 1
	if err := decodeJSON(genres, &v.Genres); err != nil {
		return v, err
	}
	if err := decodeJSON(keywords, &v.Keywords); err != nil {
		return v, err
	}
	if err := decodeJSON(people, &v.People); err != nil {
		return v, err
	}
	return v, nil
}

func scanRecommendation(row scanner) (model.Recommendation, error) {
	var v model.Recommendation
	var reasons, features, generated, expires string
	if err := row.Scan(&v.ID, &v.ServerID, &v.UserID, &v.MediaType, &v.TMDBID, &v.Title, &v.Year, &v.Overview, &v.PosterURL, &v.Score, &reasons, &features, &v.Status, &generated, &expires); err != nil {
		return v, err
	}
	var data struct {
		Components     map[string]float64 `json:"components"`
		Genres         []string           `json:"genres"`
		Keywords       []string           `json:"keywords"`
		People         []string           `json:"people"`
		Language       string             `json:"language"`
		Country        string             `json:"country"`
		RuntimeMinutes int                `json:"runtime_minutes"`
	}
	if err := decodeJSON(reasons, &v.Reasons); err != nil {
		return v, err
	}
	if err := decodeJSON(features, &data); err != nil {
		return v, err
	}
	v.Components, v.Genres, v.Keywords, v.People = data.Components, data.Genres, data.Keywords, data.People
	v.Language, v.Country, v.RuntimeMinutes = data.Language, data.Country, data.RuntimeMinutes
	var err error
	if v.GeneratedAt, err = ParseTime(generated); err != nil {
		return v, err
	}
	if v.ExpiresAt, err = ParseTime(expires); err != nil {
		return v, err
	}
	return v, nil
}

func (r *RecommendationRepository) MarkAvailable(ctx context.Context, serverID, mediaType string, tmdbID int) error {
	_, err := r.s.rw.ExecContext(ctx, `UPDATE recommendations SET status='available' WHERE server_id=? AND media_type=? AND tmdb_id=?`, serverID, mediaType, tmdbID)
	return err
}

func (r *RecommendationRepository) Prune(ctx context.Context, now time.Time) error {
	_, err := r.s.rw.ExecContext(ctx, `DELETE FROM recommendations WHERE expires_at<? AND status='active'`, FormatTime(now))
	return err
}
