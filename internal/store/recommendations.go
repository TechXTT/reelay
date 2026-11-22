package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

type RecommendationRepository struct{ s *Store }

// MaterializedRecommendationLimit is how many recommendations per user and
// media type the Jellyfin plugin shows (its RecommendationLimit default). Only
// these are recorded as impressions.
const MaterializedRecommendationLimit = 40

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
	rows, err := r.s.ro.QueryContext(ctx, `SELECT u.server_id,u.user_id,u.display_name,u.enabled,u.last_synced_at,catalog.value,activity.value
FROM jellyfin_users u
LEFT JOIN kv catalog ON catalog.key='jellyfin.catalog:'||u.server_id
LEFT JOIN kv activity ON activity.key='jellyfin.activity:'||hex(u.server_id)||':'||hex(u.user_id)
ORDER BY u.display_name`)
	if err != nil {
		return nil, fmt.Errorf("list jellyfin users: %w", err)
	}
	return collectRows(rows, func(row scanner) (model.JellyfinUser, error) {
		var value model.JellyfinUser
		var enabled int
		var synced sql.NullString
		var catalog sql.NullString
		var activity sql.NullString

		if err := row.Scan(&value.ServerID, &value.UserID, &value.DisplayName, &enabled, &synced, &catalog, &activity); err != nil {
			return value, err
		}
		value.Enabled = enabled == 1
		if t, err := scanNullTime(synced); err != nil {
			return value, err
		} else if t != nil {
			value.LastSynced = *t
		}
		var err error

		value.LastCatalogSync, err = scanNullTime(catalog)
		if err != nil {
			return value, err
		}
		value.LastActivitySync, err = scanNullTime(activity)
		return value, err
	})
}

// UserExists reports whether the Jellyfin user is known.
func (r *RecommendationRepository) UserExists(ctx context.Context, serverID, userID string) (bool, error) {
	var exists bool
	err := r.s.ro.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jellyfin_users WHERE server_id=? AND user_id=?)`, serverID, userID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check jellyfin user: %w", err)
	}
	return exists, nil
}

// UserEnabled reports whether the Jellyfin user is known and enabled in Reelay.
func (r *RecommendationRepository) UserEnabled(ctx context.Context, serverID, userID string) (bool, error) {
	var enabled bool
	err := r.s.ro.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jellyfin_users WHERE server_id=? AND user_id=? AND enabled=1)`, serverID, userID).Scan(&enabled)
	if err != nil {
		return false, fmt.Errorf("check enabled jellyfin user: %w", err)
	}
	return enabled, nil
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

// requesterSeesItem holds for a present item ji of request mr unless the plugin
// reports per-user libraries for the requester and ji is a movie outside the
// requester's last completed pass. Without per-user data the server catalog
// remains the evidence, as with older plugins.
const requesterSeesItem = `(mr.media_type<>'movie'
 OR NOT EXISTS(SELECT 1 FROM jellyfin_user_libraries l WHERE l.server_id=mr.server_id AND l.user_id=mr.user_id)
 OR EXISTS(SELECT 1 FROM jellyfin_user_libraries l JOIN jellyfin_user_items v ON v.server_id=l.server_id AND v.user_id=l.user_id AND v.sync_token=l.sync_token
  WHERE l.server_id=mr.server_id AND l.user_id=mr.user_id AND v.item_id=ji.item_id))`

// parentAvailability limits the title-present events to requests whose parent
// item is the right evidence: movies the requester can see, and series only
// while the server's plugin does not confirm requested episodes (see
// ConfirmEpisodes).
const parentAvailability = ` AND NOT (mr.media_type='series' AND EXISTS(SELECT 1 FROM kv WHERE kv.key='jellyfin.episode_confirmation:'||mr.server_id AND kv.value='1')) AND ` + requesterSeesItem

// queueTitleAvailability queues the title-present events for the requests that
// match filter (a condition on mr with filterArgs), at most one per request in
// each outbox. The per-user event goes first, and only while no global event
// exists, so a destination saved after the title became available does not
// replay it.
func queueTitleAvailability(ctx context.Context, tx *sql.Tx, now, filter string, filterArgs ...any) error {
	const payload = `SELECT mr.id,MIN(ji.item_id),json_object('event','title_available','request_id',mr.id,'server_id',mr.server_id,'user_id',mr.user_id,'media_type',mr.media_type,'tmdb_id',mr.tmdb_id,'title',mr.title,'jellyfin_item_id',MIN(ji.item_id)),?,?
 FROM media_requests mr JOIN jellyfin_items ji ON ji.server_id=mr.server_id AND ji.media_type=mr.media_type AND ji.tmdb_id=mr.tmdb_id AND ji.present=1`
	var args = append([]any{now, now}, filterArgs...)

	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_availability_outbox(request_id,item_id,payload,created_at,next_attempt_at)
 `+payload+`
 JOIN notification_destinations nd ON nd.server_id=mr.server_id AND nd.user_id=mr.user_id AND nd.enabled=1
 WHERE `+filter+` AND mr.cancelled_at IS NULL AND NOT EXISTS(SELECT 1 FROM availability_outbox ao WHERE ao.request_id=mr.id)
   AND NOT EXISTS(SELECT 1 FROM user_availability_outbox uo WHERE uo.request_id=mr.id)`+parentAvailability+` GROUP BY mr.id`, args...)
	if err != nil {
		return fmt.Errorf("queue user title availability: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO availability_outbox(request_id,item_id,payload,created_at,next_attempt_at)
 `+payload+`
 WHERE `+filter+` AND mr.cancelled_at IS NULL AND NOT EXISTS(SELECT 1 FROM availability_outbox ao WHERE ao.request_id=mr.id)`+parentAvailability+` GROUP BY mr.id`, args...)
	if err != nil {
		return fmt.Errorf("queue title availability: %w", err)
	}
	return nil
}

func (r *RecommendationRepository) CompleteSync(ctx context.Context, serverID, syncToken string) (int, error) {
	if strings.TrimSpace(serverID) == "" || strings.TrimSpace(syncToken) == "" {
		return 0, errors.New("complete jellyfin sync requires server id and sync token")
	}
	var count int64
	var err = r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = FormatTime(r.s.nowUTC())
		var result, err = tx.ExecContext(ctx, `UPDATE jellyfin_items SET present=0,updated_at=? WHERE server_id=? AND sync_token<>? AND present=1`, now, serverID, syncToken)

		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if err := queueTitleAvailability(ctx, tx, now, "mr.server_id=?", serverID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES('jellyfin.catalog:'||?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, serverID, now, now)
		return err
	})
	return int(count), err
}

func (r *RecommendationRepository) CompleteActivitySync(ctx context.Context, serverID, userID string) error {
	var now = FormatTime(r.s.nowUTC())
	var result, err = r.s.rw.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at)
SELECT 'jellyfin.activity:'||hex(server_id)||':'||hex(user_id),?,? FROM jellyfin_users WHERE server_id=? AND user_id=?
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, now, now, serverID, userID)

	if err != nil {
		return fmt.Errorf("complete Jellyfin activity sync: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *RecommendationRepository) AddActivities(ctx context.Context, events []model.JellyfinActivity) (int, error) {
	inserted := 0
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		for _, event := range events {
			if event.EventID == "" || event.ServerID == "" || event.UserID == "" || event.ItemID == "" || event.OccurredAt.IsZero() {
				return errors.New("invalid jellyfin activity")
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO jellyfin_activity(event_id,server_id,user_id,item_id,event_type,progress,occurred_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, event.EventID, event.ServerID, event.UserID, event.ItemID, event.EventType, event.Progress, FormatTime(event.OccurredAt))
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
	rows, err := r.s.ro.QueryContext(ctx, `WITH signals AS (
 SELECT item_id, MAX(occurred_at) AS latest
 FROM jellyfin_activity WHERE server_id=? AND user_id=?
 GROUP BY item_id
 HAVING MAX(event_type IN ('favorite','like','completed') OR (event_type='rating' AND progress>=0.6))=1
    AND MAX(event_type='dislike' OR (event_type='rating' AND progress<0.6))=0
)
SELECT i.server_id,i.item_id,i.media_type,i.tmdb_id,i.tvdb_id,i.imdb_id,i.title,i.year,i.genres_json,i.keywords_json,i.people_json,i.language,i.country,i.runtime_minutes,i.present
FROM jellyfin_items i JOIN signals ON signals.item_id=i.item_id
WHERE i.server_id=? AND i.media_type=? AND i.tmdb_id>0
ORDER BY signals.latest DESC LIMIT ?`, serverID, userID, serverID, mediaType, limit)
	if err != nil {
		return nil, fmt.Errorf("list recommendation seeds: %w", err)
	}
	return collectRows(rows, scanJellyfinItem)
}

func (r *RecommendationRepository) ExcludedTMDBIDs(ctx context.Context, serverID, userID, mediaType string) (map[int]bool, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT tmdb_id FROM jellyfin_items WHERE server_id=? AND media_type=? AND present=1 AND tmdb_id>0
UNION SELECT i.tmdb_id FROM jellyfin_items i JOIN jellyfin_activity a ON a.server_id=i.server_id AND a.item_id=i.item_id
  WHERE i.server_id=? AND i.media_type=? AND i.tmdb_id>0 AND a.user_id=? AND a.event_type IN ('played','completed')
UNION SELECT tmdb_id FROM recommendation_feedback WHERE server_id=? AND user_id=? AND media_type=?
UNION SELECT tmdb_id FROM recommendation_ratings WHERE server_id=? AND user_id=? AND media_type=?`,
		serverID, mediaType, serverID, mediaType, userID, serverID, userID, mediaType, serverID, userID, mediaType)
	if err != nil {
		return nil, fmt.Errorf("list excluded recommendation ids: %w", err)
	}
	return collectSet[int](rows)
}

func (r *RecommendationRepository) Ratings(ctx context.Context, serverID, userID, mediaType string) ([]model.RecommendationRating, error) {
	rows, err := r.s.ro.QueryContext(ctx, `WITH rating_values(server_id,user_id,media_type,tmdb_id,rating,updated_at) AS (
  SELECT server_id,user_id,media_type,tmdb_id,rating,updated_at FROM recommendation_ratings
  WHERE server_id=? AND user_id=? AND media_type=?
  UNION ALL
  SELECT i.server_id,a.user_id,i.media_type,i.tmdb_id,CAST(ROUND(a.progress*5) AS INTEGER),a.occurred_at
  FROM jellyfin_activity a JOIN jellyfin_items i ON i.server_id=a.server_id AND i.item_id=a.item_id
  WHERE a.server_id=? AND a.user_id=? AND i.media_type=? AND i.tmdb_id>0 AND a.event_type='rating'
    AND NOT EXISTS (SELECT 1 FROM jellyfin_activity newer WHERE newer.server_id=a.server_id AND newer.user_id=a.user_id
      AND newer.item_id=a.item_id AND newer.event_type='rating' AND newer.occurred_at>a.occurred_at)
)
SELECT server_id,user_id,media_type,tmdb_id,rating,updated_at FROM rating_values ORDER BY updated_at DESC LIMIT 50`,
		serverID, userID, mediaType, serverID, userID, mediaType)
	if err != nil {
		return nil, fmt.Errorf("list recommendation ratings: %w", err)
	}
	return collectRows(rows, scanRecommendationRating)
}

func (r *RecommendationRepository) OwnedTMDBIDs(ctx context.Context, serverID, mediaType string) (map[int]bool, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT tmdb_id FROM jellyfin_items WHERE server_id=? AND media_type=? AND present=1 AND tmdb_id>0`, serverID, mediaType)
	if err != nil {
		return nil, err
	}
	return collectSet[int](rows)
}

func (r *RecommendationRepository) Replace(ctx context.Context, serverID, userID, mediaType string, values []model.Recommendation) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		return replaceRecommendations(ctx, tx, serverID, userID, mediaType, values)
	})
}

// ReplaceOffered replaces the recommendations like Replace and, in the same
// transaction, records the best of them as offered to the user.
func (r *RecommendationRepository) ReplaceOffered(ctx context.Context, serverID, userID, mediaType string, values []model.Recommendation) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		if err := replaceRecommendations(ctx, tx, serverID, userID, mediaType, values); err != nil {
			return err
		}
		return recordImpressions(ctx, tx, serverID, userID, mediaType, values)
	})
}

func replaceRecommendations(ctx context.Context, tx *sql.Tx, serverID, userID, mediaType string, values []model.Recommendation) error {
	for _, value := range values {
		reasons, _ := encodeJSON(value.Reasons)
		features := recommendationFeatures(value, false)
		_, err := tx.ExecContext(ctx, `INSERT INTO recommendations(server_id,user_id,media_type,tmdb_id,title,year,overview,poster_url,score,reasons_json,features_json,status,generated_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id,user_id,media_type,tmdb_id) DO UPDATE SET title=excluded.title,year=excluded.year,overview=excluded.overview,poster_url=excluded.poster_url,score=excluded.score,reasons_json=excluded.reasons_json,features_json=excluded.features_json,generated_at=excluded.generated_at,expires_at=excluded.expires_at,status=CASE WHEN recommendations.status IN ('dismissed','requested','available') THEN recommendations.status ELSE 'active' END`,
			serverID, userID, mediaType, value.TMDBID, value.Title, value.Year, value.Overview, value.PosterURL, value.Score, reasons, features, "active", FormatTime(value.GeneratedAt), FormatTime(value.ExpiresAt))
		if err != nil {
			return fmt.Errorf("store recommendation %d: %w", value.TMDBID, err)
		}
	}
	query := `DELETE FROM recommendations WHERE server_id=? AND user_id=? AND media_type=? AND status='active' AND COALESCE(json_extract(features_json,'$.direction_pick'),0)=0`
	args := []any{serverID, userID, mediaType}
	if len(values) > 0 {
		query += ` AND tmdb_id NOT IN (` + placeholders(len(values)) + `)`
		for _, value := range values {
			args = append(args, value.TMDBID)
		}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("prune stale recommendations: %w", err)
	}
	return nil
}

// recordImpressions notes the best MaterializedRecommendationLimit titles by
// score as offered. A repeat offer keeps first_offered_at and first_score and
// only improves best_rank.
func recordImpressions(ctx context.Context, tx *sql.Tx, serverID, userID, mediaType string, values []model.Recommendation) error {
	var ranked = slices.Clone(values)

	slices.SortStableFunc(ranked, func(a, b model.Recommendation) int {
		if a.Score != b.Score {
			return cmp.Compare(b.Score, a.Score)
		}
		return cmp.Compare(a.TMDBID, b.TMDBID)
	})
	if len(ranked) > MaterializedRecommendationLimit {
		ranked = ranked[:MaterializedRecommendationLimit]
	}
	for i, value := range ranked {
		var at = FormatTime(value.GeneratedAt)

		if _, err := tx.ExecContext(ctx, `INSERT INTO recommendation_impressions(server_id,user_id,media_type,tmdb_id,first_offered_at,last_offered_at,best_rank,first_score)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(server_id,user_id,media_type,tmdb_id) DO UPDATE SET last_offered_at=excluded.last_offered_at,best_rank=MIN(best_rank,excluded.best_rank)`,
			serverID, userID, mediaType, value.TMDBID, at, at, i+1, value.Score); err != nil {
			return fmt.Errorf("record impression %d: %w", value.TMDBID, err)
		}
	}
	return nil
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
	return r.list(ctx, serverID, userID, mediaType, status, limit, offset)
}

// ListActive returns up to limit active recommendations of one media type,
// best first. Unlike List it is not capped at 100 rows, so session steering
// can re-rank the whole stored list; callers bound limit themselves.
func (r *RecommendationRepository) ListActive(ctx context.Context, serverID, userID, mediaType string, limit int) ([]model.Recommendation, error) {
	return r.list(ctx, serverID, userID, mediaType, "active", limit, 0)
}

func (r *RecommendationRepository) list(ctx context.Context, serverID, userID, mediaType, status string, limit, offset int) ([]model.Recommendation, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT id,server_id,user_id,media_type,tmdb_id,title,year,overview,poster_url,score,reasons_json,features_json,status,generated_at,expires_at FROM recommendations WHERE server_id=? AND user_id=? AND (?='' OR media_type=?) AND status=? AND (status<>'active' OR COALESCE(json_extract(features_json,'$.direction_pick'),0)=0) ORDER BY score DESC,id LIMIT ? OFFSET ?`, serverID, userID, mediaType, mediaType, status, limit, offset)
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

func (r *RecommendationRepository) RecordRating(ctx context.Context, id int64, actionID string, rating int) (model.Recommendation, error) {
	v, err := r.Get(ctx, id)
	if err != nil {
		return v, err
	}
	if strings.TrimSpace(actionID) == "" || rating < 1 || rating > 5 {
		return v, errors.New("recommendation rating requires an action id and a value from 1 to 5")
	}
	now := FormatTime(r.s.nowUTC())
	err = r.s.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO recommendation_ratings(action_id,server_id,user_id,media_type,tmdb_id,rating,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(server_id,user_id,media_type,tmdb_id) DO UPDATE SET action_id=excluded.action_id,rating=excluded.rating,updated_at=excluded.updated_at`,
			actionID, v.ServerID, v.UserID, v.MediaType, v.TMDBID, rating, now, now)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE recommendations SET status='dismissed' WHERE id=?`, id)
		return err
	})
	return v, err
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
		VoteAverage    float64            `json:"vote_average"`
		VoteCount      int                `json:"vote_count"`

		// Sources is absent from rows stored before provenance was recorded.
		Sources []model.RecommendationSource `json:"sources"`
	}
	if err := decodeJSON(reasons, &v.Reasons); err != nil {
		return v, err
	}
	if err := decodeJSON(features, &data); err != nil {
		return v, err
	}
	v.Components, v.Genres, v.Keywords, v.People = data.Components, data.Genres, data.Keywords, data.People
	v.Language, v.Country, v.RuntimeMinutes = data.Language, data.Country, data.RuntimeMinutes
	v.VoteAverage, v.VoteCount = data.VoteAverage, data.VoteCount
	v.Sources = data.Sources
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
	if err != nil {
		return err
	}
	_, err = r.s.rw.ExecContext(ctx, `DELETE FROM recommendation_concentration WHERE generated_at<?`, FormatTime(now.AddDate(0, 0, -concentrationRetentionDays)))
	return err
}
