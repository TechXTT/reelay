package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
	return r.CreateForAction(ctx, value, false)
}

func (r *RequestRepository) CreateForAction(ctx context.Context, value model.MediaRequest, reopen bool) (bool, error) {
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
 cancelled_at=CASE WHEN ? THEN NULL ELSE media_requests.cancelled_at END,
 monitor_mode=CASE WHEN media_requests.monitor_mode='all' THEN 'all'
                   WHEN excluded.monitor_mode='all' OR (media_requests.monitor_mode='future_only' AND excluded.monitor_mode='latest_season') THEN excluded.monitor_mode
                   WHEN media_requests.monitor_mode='' THEN excluded.monitor_mode ELSE media_requests.monitor_mode END`, value.ServerID, value.UserID, value.MediaType, value.TMDBID, value.Title, value.Year,
		FormatTime(value.RequestedAt), value.MonitorMode, value.SubjectType, value.SubjectID, reopen)
	if err != nil {
		return false, fmt.Errorf("record media request: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *RequestRepository) Get(ctx context.Context, id int64) (model.MediaRequest, error) {
	var request model.MediaRequest
	var cancelled sql.NullString
	var requested, seasons string
	var err = r.s.ro.QueryRowContext(ctx, `SELECT `+requestIdentityColumns+`,cancelled_at,seasons_json FROM media_requests WHERE id=?`, id).Scan(
		append(requestIdentityTargets(&request, &requested), &cancelled, &seasons)...)

	if errors.Is(err, sql.ErrNoRows) {
		return request, ErrNotFound
	}
	if err != nil {
		return request, err
	}
	if err := finishRequest(&request, requested, cancelled, seasons); err != nil {
		return request, err
	}
	request.Trial, err = r.Trial(ctx, id)
	return request, err
}

const requestIdentityColumns = `id,server_id,user_id,media_type,tmdb_id,title,year,requested_at,monitor_mode,subject_type,subject_id`

func requestIdentityTargets(request *model.MediaRequest, requested *string) []any {
	return []any{&request.ID, &request.ServerID, &request.UserID, &request.MediaType, &request.TMDBID, &request.Title, &request.Year, requested, &request.MonitorMode, &request.SubjectType, &request.SubjectID}
}

// finishRequest parses the textual columns shared by every media request read.
func finishRequest(request *model.MediaRequest, requested string, cancelled sql.NullString, seasons string) error {
	var err error

	request.RequestedAt, err = ParseTime(requested)
	if err != nil {
		return err
	}
	request.CancelledAt, err = scanNullTime(cancelled)
	if err != nil {
		return err
	}
	return decodeJSON(seasons, &request.Seasons)
}

// Cancelling withdraws this requester's subscription. It never deletes or
// interrupts the shared media subject or another requester's download.
func (r *RequestRepository) Cancel(ctx context.Context, id int64) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var result, err = tx.ExecContext(ctx, `UPDATE media_requests SET cancelled_at=COALESCE(cancelled_at,?) WHERE id=?`, FormatTime(r.s.nowUTC()), id)

		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM availability_outbox WHERE request_id=? AND delivered_at IS NULL`, id)
		return err
	})
}

func (r *RequestRepository) Reactivate(ctx context.Context, id int64) error {
	var result, err = r.s.rw.ExecContext(ctx, `UPDATE media_requests SET cancelled_at=NULL WHERE id=?`, id)

	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *RequestRepository) AddSeasons(ctx context.Context, serverID, userID string, tmdbID int, seriesID int64, seasons []int) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var raw string
		var selected []int
		var err = tx.QueryRowContext(ctx, `SELECT seasons_json FROM media_requests WHERE server_id=? AND user_id=? AND media_type='series' AND tmdb_id=?`, serverID, userID, tmdbID).Scan(&raw)

		if err != nil {
			return err
		}
		if err := decodeJSON(raw, &selected); err != nil {
			return err
		}
		for _, season := range seasons {
			if season < 0 || season > 999 {
				return errors.New("season must be between 0 and 999")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO series_seasons(series_id,season) VALUES(?,?)`, seriesID, season); err != nil {
				return err
			}
			if !slices.Contains(selected, season) {
				selected = append(selected, season)
			}
		}
		slices.Sort(selected)
		raw, err = encodeJSON(selected)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE media_requests SET seasons_json=? WHERE server_id=? AND user_id=? AND media_type='series' AND tmdb_id=?`, raw, serverID, userID, tmdbID)
		return err
	})
}

func (r *RequestRepository) SelectedSeasons(ctx context.Context, seriesID int64) ([]int, error) {
	var rows, err = r.s.ro.QueryContext(ctx, `SELECT season FROM series_seasons WHERE series_id=? ORDER BY season`, seriesID)

	if err != nil {
		return nil, err
	}
	return collectRows(rows, scanColumn[int])
}

func (r *RequestRepository) List(ctx context.Context, serverID, userID string) ([]model.MediaRequest, error) {
	return r.ListPage(ctx, serverID, userID, 0, false)
}

func (r *RequestRepository) ListPage(ctx context.Context, serverID, userID string, offset int, attention bool) ([]model.MediaRequest, error) {
	if offset < 0 || offset > 1000000 {
		return nil, errors.New("request offset is out of bounds")
	}
	if strings.TrimSpace(serverID) == "" || strings.TrimSpace(userID) == "" {
		return nil, errors.New("list media requests requires server_id and user_id")
	}
	rows, err := r.s.ro.QueryContext(ctx, `WITH selected_requests AS MATERIALIZED (
 SELECT * FROM media_requests mr WHERE server_id=? AND user_id=? AND (?=0 OR (cancelled_at IS NULL AND (
 EXISTS(SELECT 1 FROM movies m WHERE mr.subject_type='movie' AND m.id=mr.subject_id AND m.tmdb_id=mr.tmdb_id AND m.state IN ('failed','import_failed'))
 OR EXISTS(SELECT 1 FROM series sr JOIN episodes ep ON ep.series_id=sr.id WHERE mr.subject_type='series' AND sr.id=mr.subject_id AND sr.tmdb_id=mr.tmdb_id AND ep.state IN ('failed','import_failed') AND (mr.monitor_mode<>'' OR mr.seasons_json='[]' OR ep.season IN (SELECT value FROM json_each(mr.seasons_json))) AND (NOT EXISTS(SELECT 1 FROM series_trials t WHERE t.request_id=mr.id AND t.decision<>'continue') OR EXISTS(SELECT 1 FROM series_trial_episodes te WHERE te.request_id=mr.id AND te.episode_id=ep.id)))
 ))) ORDER BY requested_at DESC,id DESC LIMIT ? OFFSET ?
), selected_series AS MATERIALIZED (
 SELECT request.id AS request_id,sr.id AS subject_id,request.seasons_json,request.monitor_mode FROM selected_requests request JOIN series sr ON sr.id=request.subject_id AND sr.tmdb_id=request.tmdb_id
 WHERE request.subject_type='series'
), selected_movies AS MATERIALIZED (
 SELECT DISTINCT m.id AS subject_id FROM selected_requests request JOIN movies m ON m.id=request.subject_id AND m.tmdb_id=request.tmdb_id
 WHERE request.subject_type='movie'
), episode_summary AS (
 SELECT selected.request_id,COUNT(*) AS total,
        SUM(CASE WHEN ep.state='imported' THEN 1 ELSE 0 END) AS imported,
        SUM(CASE WHEN ep.state IN ('failed','import_failed') THEN 1 ELSE 0 END) AS failed,
        SUM(CASE WHEN ep.state IN ('grabbed','downloading','importing') THEN 1 ELSE 0 END) AS active,
        SUM(CASE WHEN ep.state IN ('wanted','searching') THEN 1 ELSE 0 END) AS wanted,
        SUM(CASE WHEN ep.state<>'unmonitored' THEN 1 ELSE 0 END) AS monitored,
        SUM(CASE WHEN ep.air_date>? THEN 1 ELSE 0 END) AS upcoming,
        MAX(NULLIF(ep.last_error,'')) AS last_error,
        MIN(ep.next_search_at) AS next_search_at
 FROM episodes ep JOIN selected_series selected ON selected.subject_id=ep.series_id
 WHERE (selected.monitor_mode<>'' OR selected.seasons_json='[]' OR ep.season IN (SELECT value FROM json_each(selected.seasons_json))) AND (NOT EXISTS(SELECT 1 FROM series_trials t WHERE t.request_id=selected.request_id AND t.decision<>'continue') OR EXISTS(SELECT 1 FROM series_trial_episodes te WHERE te.request_id=selected.request_id AND te.episode_id=ep.id)) GROUP BY selected.request_id
), movie_progress AS (
 SELECT subject_id,MAX(progress) AS progress FROM grabs
 WHERE subject_type='movie' AND subject_id IN (SELECT subject_id FROM selected_movies)
   AND state IN ('pending','downloading','completed','importing') GROUP BY subject_id
), series_progress AS (
 SELECT selected.request_id,MAX(g.progress) AS progress FROM grabs g JOIN episodes ep ON g.subject_type='episode' AND g.subject_id=ep.id
 JOIN selected_series selected ON selected.subject_id=ep.series_id
 WHERE g.state IN ('pending','downloading','completed','importing') AND (selected.monitor_mode<>'' OR selected.seasons_json='[]' OR ep.season IN (SELECT value FROM json_each(selected.seasons_json))) AND (NOT EXISTS(SELECT 1 FROM series_trials t WHERE t.request_id=selected.request_id AND t.decision<>'continue') OR EXISTS(SELECT 1 FROM series_trial_episodes te WHERE te.request_id=selected.request_id AND te.episode_id=ep.id)) GROUP BY selected.request_id
)
SELECT
 mr.id,mr.server_id,mr.user_id,mr.media_type,mr.tmdb_id,mr.title,mr.year,mr.requested_at,mr.monitor_mode,mr.subject_type,mr.subject_id,
	CASE WHEN mr.cancelled_at IS NOT NULL THEN 'cancelled' WHEN mr.media_type='movie' THEN COALESCE(m.state,'missing')
	     ELSE CASE WHEN sr.id IS NULL THEN 'missing'
	               WHEN sr.status IN ('paused','ended') THEN sr.status
	               WHEN COALESCE(e.total,0)=0 THEN 'waiting_metadata'
	              WHEN e.failed>0 THEN 'attention_needed'
	              WHEN e.active>0 THEN 'downloading'
	              WHEN e.wanted>0 THEN 'searching'
	              WHEN e.imported=e.total THEN 'imported'
	              WHEN e.imported>0 THEN 'partially_imported'
	              WHEN e.upcoming>0 THEN 'waiting_air_date'
	              WHEN sr.monitor_mode='none' THEN 'not_monitored'
	              ELSE 'not_monitored' END END,
	CASE WHEN mr.media_type='movie' THEN CASE WHEN m.state='imported' THEN 1 ELSE COALESCE(mp.progress,0) END
     WHEN e.imported=e.total AND COALESCE(e.total,0)>0 THEN 1 ELSE COALESCE(sp.progress,0) END,
 CASE WHEN mr.media_type='movie' THEN COALESCE(m.last_error,'') ELSE COALESCE(e.last_error,'') END,
 CASE WHEN mr.media_type='movie' THEN m.next_search_at ELSE e.next_search_at END,
 EXISTS(SELECT 1 FROM jellyfin_items ji WHERE ji.server_id=mr.server_id AND ji.media_type=mr.media_type AND ji.tmdb_id=mr.tmdb_id AND ji.present=1),
 COALESCE(e.imported,0),COALESCE(e.total,0),mr.cancelled_at,mr.seasons_json,
 COALESCE((SELECT ji.item_id FROM jellyfin_items ji WHERE ji.server_id=mr.server_id AND ji.media_type=mr.media_type AND ji.tmdb_id=mr.tmdb_id AND ji.present=1 ORDER BY ji.item_id LIMIT 1),'')
FROM selected_requests mr
LEFT JOIN movies m ON mr.subject_type='movie' AND m.id=mr.subject_id AND m.tmdb_id=mr.tmdb_id
LEFT JOIN series sr ON mr.subject_type='series' AND sr.id=mr.subject_id AND sr.tmdb_id=mr.tmdb_id
LEFT JOIN episode_summary e ON mr.subject_type='series' AND e.request_id=mr.id
LEFT JOIN movie_progress mp ON mp.subject_id=m.id AND mr.subject_type='movie'
LEFT JOIN series_progress sp ON sp.request_id=mr.id AND mr.subject_type='series'
ORDER BY mr.requested_at DESC,mr.id DESC`, serverID, userID, attention, model.MediaRequestListLimit, offset, FormatTime(r.s.nowUTC()))
	if err != nil {
		return nil, fmt.Errorf("list media requests: %w", err)
	}
	values, err := collectRows(rows, scanListedRequest)
	if err != nil {
		return nil, err
	}
	var ids = make([]int64, len(values))
	for i, value := range values {
		ids[i] = value.ID
	}
	trials, err := r.trialsByRequest(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range values {
		values[i].Trial = trials[values[i].ID]
	}
	return values, nil
}

func scanListedRequest(row scanner) (model.MediaRequest, error) {
	var value model.MediaRequest
	var requested, seasons string
	var next, cancelled sql.NullString
	var available int

	if err := row.Scan(append(requestIdentityTargets(&value, &requested), &value.State, &value.Progress, &value.LastError,
		&next, &available, &value.ImportedEpisodes, &value.TotalEpisodes, &cancelled, &seasons, &value.JellyfinItemID)...); err != nil {
		return value, err
	}
	if err := finishRequest(&value, requested, cancelled, seasons); err != nil {
		return value, err
	}
	var err error
	value.NextSearchAt, err = scanNullTime(next)
	value.Available = available == 1
	return value, err
}

// SelectedSeasonsBySeries returns the selected seasons of every listed series keyed by series ID.
func (r *RequestRepository) SelectedSeasonsBySeries(ctx context.Context, seriesIDs []int64) (map[int64][]int, error) {
	seasons := make(map[int64][]int, len(seriesIDs))
	for _, chunk := range chunkIDs(seriesIDs) {
		rows, err := r.s.ro.QueryContext(ctx, `SELECT series_id, season FROM series_seasons WHERE series_id IN (`+placeholders(len(chunk))+`) ORDER BY series_id, season`, int64Args(chunk)...)
		if err != nil {
			return nil, err
		}
		pairs, err := collectRows(rows, func(row scanner) ([2]int64, error) {
			var pair [2]int64
			err := row.Scan(&pair[0], &pair[1])
			return pair, err
		})
		if err != nil {
			return nil, err
		}
		for _, pair := range pairs {
			seasons[pair[0]] = append(seasons[pair[0]], int(pair[1]))
		}
	}
	return seasons, nil
}
