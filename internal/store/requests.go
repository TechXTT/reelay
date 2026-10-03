package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

var ErrActionIDConflict = errors.New("request action id conflict")

type RequestRepository struct{ s *Store }

func (s *Store) Requests() *RequestRepository { return &RequestRepository{s: s} }

func (r *RequestRepository) ActionRecorded(ctx context.Context, actionID, serverID, userID, mediaType string, tmdbID int) (bool, error) {
	var action, storedServerID, storedUserID, storedMediaType string
	var storedTMDBID int
	err := r.s.ro.QueryRowContext(ctx, `SELECT action,server_id,user_id,media_type,tmdb_id FROM recommendation_feedback WHERE action_id=?`, actionID).Scan(&action, &storedServerID, &storedUserID, &storedMediaType, &storedTMDBID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check request action: %w", err)
	}
	if action != "request" || storedServerID != serverID || storedUserID != userID || storedMediaType != mediaType || storedTMDBID != tmdbID {
		return false, fmt.Errorf("%w: %q is already recorded for a different action or title", ErrActionIDConflict, actionID)
	}
	return true, nil
}

func (r *RequestRepository) Create(ctx context.Context, value model.MediaRequest) (bool, error) {
	if value.ServerID == "" || value.UserID == "" || value.TMDBID <= 0 || value.Title == "" || value.SubjectID <= 0 {
		return false, errors.New("media request requires server, user, title, tmdb id, and subject")
	}
	if value.MediaType != "movie" && value.MediaType != "series" {
		return false, errors.New("media request has invalid media type")
	}
	if value.SubjectType != value.MediaType {
		return false, errors.New("media request subject type must match media type")
	}
	if value.MediaType == "series" && value.MonitorMode != "" && value.MonitorMode != string(model.MonitorLatestSeason) && value.MonitorMode != string(model.MonitorAll) && value.MonitorMode != string(model.MonitorFutureOnly) {
		return false, fmt.Errorf("invalid series monitor mode %q", value.MonitorMode)
	}
	if value.RequestedAt.IsZero() {
		value.RequestedAt = r.s.nowUTC()
	}
	res, err := r.s.rw.ExecContext(ctx, `INSERT OR IGNORE INTO media_requests
(server_id,user_id,media_type,tmdb_id,title,year,requested_at,monitor_mode,subject_type,subject_id)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(server_id,user_id,media_type,tmdb_id) DO UPDATE SET
 title=excluded.title,year=excluded.year,subject_type=excluded.subject_type,subject_id=excluded.subject_id,
 monitor_mode=CASE WHEN media_requests.monitor_mode='all' THEN 'all'
                   WHEN excluded.monitor_mode='all' OR (media_requests.monitor_mode='future_only' AND excluded.monitor_mode='latest_season') THEN excluded.monitor_mode
                   WHEN media_requests.monitor_mode='' THEN excluded.monitor_mode ELSE media_requests.monitor_mode END`, value.ServerID, value.UserID, value.MediaType, value.TMDBID, value.Title, value.Year,
		FormatTime(value.RequestedAt), value.MonitorMode, value.SubjectType, value.SubjectID)
	if err != nil {
		return false, fmt.Errorf("record media request: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *RequestRepository) List(ctx context.Context, serverID, userID string) ([]model.MediaRequest, error) {
	if strings.TrimSpace(serverID) == "" || strings.TrimSpace(userID) == "" {
		return nil, errors.New("list media requests requires server_id and user_id")
	}
	rows, err := r.s.ro.QueryContext(ctx, `WITH selected_requests AS MATERIALIZED (
 SELECT * FROM media_requests WHERE server_id=? AND user_id=? ORDER BY requested_at DESC,id DESC LIMIT ?
), selected_series AS MATERIALIZED (
 SELECT DISTINCT sr.id AS subject_id FROM selected_requests request JOIN series sr ON sr.id=request.subject_id AND sr.tmdb_id=request.tmdb_id
 WHERE request.subject_type='series'
), selected_movies AS MATERIALIZED (
 SELECT DISTINCT m.id AS subject_id FROM selected_requests request JOIN movies m ON m.id=request.subject_id AND m.tmdb_id=request.tmdb_id
 WHERE request.subject_type='movie'
), episode_summary AS (
 SELECT ep.series_id,COUNT(*) AS total,
        SUM(CASE WHEN ep.state='imported' THEN 1 ELSE 0 END) AS imported,
        SUM(CASE WHEN ep.state IN ('failed','import_failed') THEN 1 ELSE 0 END) AS failed,
        SUM(CASE WHEN ep.state IN ('grabbed','downloading','importing') THEN 1 ELSE 0 END) AS active,
        SUM(CASE WHEN ep.state IN ('wanted','searching') THEN 1 ELSE 0 END) AS wanted,
        SUM(CASE WHEN ep.state<>'unmonitored' THEN 1 ELSE 0 END) AS monitored,
        SUM(CASE WHEN ep.air_date>? THEN 1 ELSE 0 END) AS upcoming,
        MAX(NULLIF(ep.last_error,'')) AS last_error,
        MIN(ep.next_search_at) AS next_search_at
 FROM episodes ep JOIN selected_series selected ON selected.subject_id=ep.series_id GROUP BY ep.series_id
), movie_progress AS (
 SELECT subject_id,MAX(progress) AS progress FROM grabs
 WHERE subject_type='movie' AND subject_id IN (SELECT subject_id FROM selected_movies)
   AND state IN ('pending','downloading','completed','importing') GROUP BY subject_id
), series_progress AS (
 SELECT ep.series_id,MAX(g.progress) AS progress FROM grabs g JOIN episodes ep ON g.subject_type='episode' AND g.subject_id=ep.id
 JOIN selected_series selected ON selected.subject_id=ep.series_id
 WHERE g.state IN ('pending','downloading','completed','importing') GROUP BY ep.series_id
)
SELECT
 mr.id,mr.server_id,mr.user_id,mr.media_type,mr.tmdb_id,mr.title,mr.year,mr.requested_at,mr.monitor_mode,mr.subject_type,mr.subject_id,
	CASE WHEN mr.media_type='movie' THEN COALESCE(m.state,'missing')
	     ELSE CASE WHEN sr.id IS NULL THEN 'missing'
	               WHEN sr.status IN ('paused','ended') THEN sr.status
	               WHEN COALESCE(e.total,0)=0 THEN 'waiting_metadata'
	              WHEN e.failed>0 THEN 'attention_needed'
	              WHEN e.active>0 THEN 'downloading'
	              WHEN e.wanted>0 THEN 'searching'
	              WHEN e.imported=e.total THEN 'imported'
	              WHEN e.imported>0 THEN 'partially_imported'
	              WHEN sr.monitor_mode='none' THEN 'not_monitored'
	              WHEN e.upcoming>0 THEN 'waiting_air_date'
	              ELSE 'not_monitored' END END,
	CASE WHEN mr.media_type='movie' THEN CASE WHEN m.state='imported' THEN 1 ELSE COALESCE(mp.progress,0) END
     WHEN e.imported=e.total AND COALESCE(e.total,0)>0 THEN 1 ELSE COALESCE(sp.progress,0) END,
 CASE WHEN mr.media_type='movie' THEN COALESCE(m.last_error,'') ELSE COALESCE(e.last_error,'') END,
 CASE WHEN mr.media_type='movie' THEN m.next_search_at ELSE e.next_search_at END,
 EXISTS(SELECT 1 FROM jellyfin_items ji WHERE ji.server_id=mr.server_id AND ji.media_type=mr.media_type AND ji.tmdb_id=mr.tmdb_id AND ji.present=1),
 COALESCE(e.imported,0),COALESCE(e.total,0)
FROM selected_requests mr
LEFT JOIN movies m ON mr.subject_type='movie' AND m.id=mr.subject_id AND m.tmdb_id=mr.tmdb_id
LEFT JOIN series sr ON mr.subject_type='series' AND sr.id=mr.subject_id AND sr.tmdb_id=mr.tmdb_id
LEFT JOIN episode_summary e ON mr.subject_type='series' AND e.series_id=sr.id
LEFT JOIN movie_progress mp ON mp.subject_id=m.id AND mr.subject_type='movie'
LEFT JOIN series_progress sp ON sp.series_id=sr.id AND mr.subject_type='series'
ORDER BY mr.requested_at DESC,mr.id DESC`, serverID, userID, model.MediaRequestListLimit, FormatTime(r.s.nowUTC()))
	if err != nil {
		return nil, fmt.Errorf("list media requests: %w", err)
	}
	return collectRows(rows, func(row scanner) (model.MediaRequest, error) {
		var value model.MediaRequest
		var requested string
		var next sql.NullString
		var available int
		if err := row.Scan(&value.ID, &value.ServerID, &value.UserID, &value.MediaType, &value.TMDBID, &value.Title, &value.Year,
			&requested, &value.MonitorMode, &value.SubjectType, &value.SubjectID, &value.State, &value.Progress, &value.LastError,
			&next, &available, &value.ImportedEpisodes, &value.TotalEpisodes); err != nil {
			return value, err
		}
		var err error
		value.RequestedAt, err = ParseTime(requested)
		if err != nil {
			return value, err
		}
		if next.Valid {
			parsed, err := ParseTime(next.String)
			if err != nil {
				return value, err
			}
			value.NextSearchAt = &parsed
		}
		value.Available = available == 1
		return value, nil
	})
}
