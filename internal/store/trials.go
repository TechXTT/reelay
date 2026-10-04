package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/TechXTT/reelay/internal/model"
)

const selectTrialSQL = `SELECT mr.id,mr.server_id,mr.user_id,mr.tmdb_id,mr.title,t.episode_limit,t.scope,t.decision,COALESCE(t.rating,0)
FROM series_trials t JOIN media_requests mr ON mr.id=t.request_id`

func scanTrial(row scanner) (model.SeriesTrial, error) {
	var trial model.SeriesTrial
	err := row.Scan(&trial.RequestID, &trial.ServerID, &trial.UserID, &trial.TMDBID, &trial.Title, &trial.EpisodeLimit, &trial.Scope, &trial.Decision, &trial.Rating)
	trial.Episodes = []model.TrialEpisode{}
	return trial, err
}

// attachTrialEpisodes loads the episodes of every trial in one query and
// derives each trial's watched count and state.
func (r *RequestRepository) attachTrialEpisodes(ctx context.Context, trials []model.SeriesTrial) error {
	if len(trials) == 0 {
		return nil
	}
	var positions = make(map[int64]int, len(trials))
	var args = make([]any, len(trials))
	for i, trial := range trials {
		positions[trial.RequestID] = i
		args[i] = trial.RequestID
	}
	var rows, err = r.s.ro.QueryContext(ctx, `SELECT te.request_id,ep.id,ep.season,ep.number,te.watched_at IS NOT NULL FROM series_trial_episodes te JOIN episodes ep ON ep.id=te.episode_id WHERE te.request_id IN (`+placeholders(len(args))+`) ORDER BY ep.season,ep.number`, args...)

	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var requestID int64
		var episode model.TrialEpisode
		if err := rows.Scan(&requestID, &episode.ID, &episode.Season, &episode.Number, &episode.Watched); err != nil {
			return err
		}
		var trial = &trials[positions[requestID]]
		trial.Episodes = append(trial.Episodes, episode)
		if episode.Watched {
			trial.WatchedEpisodes++
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range trials {
		trials[i].State = trialState(&trials[i])
	}
	return nil
}

func trialState(trial *model.SeriesTrial) string {
	switch {
	case trial.Decision == "continue":
		return "continued"
	case trial.Decision == "stop":
		return "stopped"
	case trial.EpisodeLimit > 0 && trial.WatchedEpisodes >= trial.EpisodeLimit:
		return "awaiting_vote"
	case len(trial.Episodes) == 0:
		return "waiting_metadata"
	}
	return "watching"
}

// trialsByRequest returns the trials of the given requests keyed by request
// id using two queries regardless of how many requests are supplied.
func (r *RequestRepository) trialsByRequest(ctx context.Context, requestIDs []int64) (map[int64]*model.SeriesTrial, error) {
	var found = make(map[int64]*model.SeriesTrial, len(requestIDs))
	if len(requestIDs) == 0 {
		return found, nil
	}
	var args = make([]any, len(requestIDs))
	for i, id := range requestIDs {
		args[i] = id
	}
	var rows, err = r.s.ro.QueryContext(ctx, selectTrialSQL+` WHERE t.request_id IN (`+placeholders(len(args))+`)`, args...)

	if err != nil {
		return nil, err
	}
	trials, err := collectRows(rows, scanTrial)
	if err != nil {
		return nil, err
	}
	if err := r.attachTrialEpisodes(ctx, trials); err != nil {
		return nil, err
	}
	for i := range trials {
		found[trials[i].RequestID] = &trials[i]
	}
	return found, nil
}

func (r *RequestRepository) Trial(ctx context.Context, id int64) (*model.SeriesTrial, error) {
	var found, err = r.trialsByRequest(ctx, []int64{id})

	return found[id], err
}

func (r *RequestRepository) ActiveTrials(ctx context.Context, serverID, userID string) ([]model.SeriesTrial, error) {
	var rows, err = r.s.ro.QueryContext(ctx, selectTrialSQL+` WHERE mr.server_id=? AND mr.user_id=? AND mr.cancelled_at IS NULL AND t.decision='' ORDER BY mr.requested_at,mr.id LIMIT 100`, serverID, userID)

	if err != nil {
		return nil, err
	}
	trials, err := collectRows(rows, scanTrial)
	if err != nil {
		return nil, err
	}
	if trials == nil {
		return []model.SeriesTrial{}, nil
	}
	return trials, r.attachTrialEpisodes(ctx, trials)
}

// Trial limits belong to individual requesters; a broader shared monitor stays intact.
func (r *RequestRepository) StartTrial(ctx context.Context, serverID, userID string, tmdbID int, scope string) error {
	var limit int

	switch scope {
	case "one_episode":
		limit = 1
	case "three_episodes":
		limit = 3
	case "first_season":
		limit = 0
	default:
		return errors.New("trial must be one episode, three episodes, or the first season")
	}
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var id int64
		var mode, seasons string
		var err = tx.QueryRowContext(ctx, `SELECT mr.id,mr.monitor_mode,mr.seasons_json FROM media_requests mr JOIN series sr ON sr.id=mr.subject_id AND sr.tmdb_id=mr.tmdb_id WHERE mr.server_id=? AND mr.user_id=? AND mr.media_type='series' AND mr.tmdb_id=? AND mr.cancelled_at IS NULL AND sr.status='following'`, serverID, userID, tmdbID).Scan(&id, &mode, &seasons)

		if err != nil {
			return err
		}
		if mode != "" || seasons != "[]" {
			return errors.New("this requester already follows more than a trial")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO series_trials(request_id,scope,episode_limit) VALUES(?,?,?) ON CONFLICT(request_id) DO NOTHING`, id, scope, limit)
		if err != nil {
			return err
		}
		var existing, decision string
		if err := tx.QueryRowContext(ctx, `SELECT scope,decision FROM series_trials WHERE request_id=?`, id).Scan(&existing, &decision); err != nil {
			return err
		}
		if existing != scope || decision != "" {
			return errors.New("an existing trial cannot be replaced")
		}
		return nil
	})
}

func (r *RequestRepository) PrepareTrialEpisodes(ctx context.Context, seriesID int64) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var selections = make([][3]int64, 0)
		if _, err := tx.ExecContext(ctx, `UPDATE series_trials SET episode_limit=MIN(3,(SELECT COUNT(*) FROM episodes WHERE series_id=? AND season>0 AND number>0)) WHERE scope='three_episodes' AND decision='' AND request_id IN(SELECT id FROM media_requests WHERE subject_id=? AND subject_type='series' AND cancelled_at IS NULL)`, seriesID, seriesID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE series_trials SET season_number=COALESCE(season_number,(SELECT MIN(season) FROM episodes WHERE series_id=? AND season>0 AND number>0)) WHERE scope='first_season' AND decision='' AND request_id IN(SELECT id FROM media_requests WHERE subject_id=? AND subject_type='series' AND cancelled_at IS NULL)`, seriesID, seriesID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO series_trial_episodes(request_id,episode_id) SELECT t.request_id,ep.id FROM series_trials t JOIN media_requests mr ON mr.id=t.request_id JOIN episodes ep ON ep.series_id=mr.subject_id AND ep.season=t.season_number AND ep.number>0 WHERE t.scope='first_season' AND t.decision='' AND mr.subject_id=? AND mr.subject_type='series' AND mr.cancelled_at IS NULL`, seriesID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE series_trials SET episode_limit=(SELECT COUNT(*) FROM series_trial_episodes te WHERE te.request_id=series_trials.request_id) WHERE scope='first_season' AND decision='' AND request_id IN(SELECT id FROM media_requests WHERE subject_id=? AND subject_type='series')`, seriesID); err != nil {
			return err
		}
		var rows, err = tx.QueryContext(ctx, `SELECT t.request_id,t.episode_limit,COUNT(te.episode_id) FROM series_trials t JOIN media_requests mr ON mr.id=t.request_id LEFT JOIN series_trial_episodes te ON te.request_id=t.request_id WHERE mr.subject_id=? AND mr.subject_type='series' AND mr.cancelled_at IS NULL AND t.decision='' GROUP BY t.request_id`, seriesID)

		if err != nil {
			return err
		}
		for rows.Next() {
			var selection [3]int64
			if err := rows.Scan(&selection[0], &selection[1], &selection[2]); err != nil {
				rows.Close()
				return err
			}
			selections = append(selections, selection)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, selection := range selections {
			if selection[2] >= selection[1] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO series_trial_episodes(request_id,episode_id) SELECT ?,ep.id FROM episodes ep WHERE ep.series_id=? AND ep.season>0 AND ep.number>0 AND NOT EXISTS(SELECT 1 FROM series_trial_episodes te WHERE te.request_id=? AND te.episode_id=ep.id) ORDER BY ep.season,ep.number LIMIT ?`, selection[0], seriesID, selection[0], selection[1]-selection[2]); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *RequestRepository) TrialEpisodeIDs(ctx context.Context, seriesID int64) ([]int64, error) {
	var rows, err = r.s.ro.QueryContext(ctx, `SELECT DISTINCT te.episode_id FROM series_trial_episodes te JOIN series_trials t ON t.request_id=te.request_id JOIN media_requests mr ON mr.id=t.request_id WHERE mr.subject_id=? AND mr.subject_type='series' AND mr.cancelled_at IS NULL AND t.decision=''`, seriesID)

	if err != nil {
		return nil, err
	}
	ids, err := collectRows(rows, scanColumn[int64])
	if ids == nil {
		ids = make([]int64, 0)
	}
	return ids, err
}

func (r *RequestRepository) MarkTrialWatched(ctx context.Context, id int64, serverID, userID string, season, number int) error {
	var result, err = r.s.rw.ExecContext(ctx, `UPDATE series_trial_episodes SET watched_at=COALESCE(watched_at,?) WHERE request_id=? AND episode_id IN(SELECT id FROM episodes WHERE season=? AND number=?) AND EXISTS(SELECT 1 FROM media_requests mr JOIN series_trials t ON t.request_id=mr.id WHERE mr.id=? AND mr.server_id=? AND mr.user_id=? AND mr.cancelled_at IS NULL AND t.decision='')`, FormatTime(r.s.nowUTC()), id, season, number, id, serverID, userID)

	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return errors.New("watched episode does not belong to an active trial")
	}
	return nil
}

func (r *RequestRepository) VoteTrial(ctx context.Context, id int64, decision string, rating int) error {
	if (decision != "continue" && decision != "stop") || rating < 1 || rating > 5 {
		return errors.New("trial vote requires continue or stop and a rating from 1 to 5")
	}
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var limit, watched, existingRating, tmdbID int
		var serverID, userID, existing string
		var subjectID int64
		var cancelled sql.NullString
		var err = tx.QueryRowContext(ctx, `SELECT t.episode_limit,(SELECT COUNT(*) FROM series_trial_episodes te WHERE te.request_id=t.request_id AND te.watched_at IS NOT NULL),t.decision,COALESCE(t.rating,0),mr.server_id,mr.user_id,mr.tmdb_id,mr.subject_id,mr.cancelled_at FROM series_trials t JOIN media_requests mr ON mr.id=t.request_id WHERE mr.id=?`, id).Scan(&limit, &watched, &existing, &existingRating, &serverID, &userID, &tmdbID, &subjectID, &cancelled)

		if err != nil {
			return err
		}
		if existing != "" {
			if existing == decision && existingRating == rating {
				return nil
			}
			return errors.New("trial already has a different vote")
		}
		if cancelled.Valid {
			return errors.New("trial has been withdrawn")
		}
		if limit == 0 || watched < limit {
			return errors.New("watch every trial episode before voting")
		}
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM series WHERE id=? AND tmdb_id=?`, subjectID, tmdbID).Scan(&present); err != nil {
			return err
		}
		if present != 1 {
			return errors.New("trial series is missing")
		}
		var now = FormatTime(r.s.nowUTC())
		if _, err := tx.ExecContext(ctx, `UPDATE series_trials SET decision=?,rating=?,voted_at=? WHERE request_id=?`, decision, rating, now, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO recommendation_ratings(action_id,server_id,user_id,media_type,tmdb_id,rating,created_at,updated_at) VALUES(?,?,?,'series',?,?,?,?) ON CONFLICT(server_id,user_id,media_type,tmdb_id) DO UPDATE SET action_id=excluded.action_id,rating=excluded.rating,updated_at=excluded.updated_at`, fmt.Sprintf("trial-vote-%d", id), serverID, userID, tmdbID, rating, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recommendations SET status=CASE WHEN ?='continue' THEN 'requested' ELSE 'dismissed' END WHERE server_id=? AND user_id=? AND media_type='series' AND tmdb_id=?`, decision, serverID, userID, tmdbID); err != nil {
			return err
		}
		if decision == "continue" {
			if _, err := tx.ExecContext(ctx, `UPDATE series SET monitor_mode='all',status='following' WHERE id=? AND tmdb_id=?`, subjectID, tmdbID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE media_requests SET monitor_mode='all' WHERE id=?`, id)
		} else {
			if _, err := tx.ExecContext(ctx, `DELETE FROM availability_outbox WHERE request_id=? AND delivered_at IS NULL`, id); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `UPDATE media_requests SET cancelled_at=? WHERE id=?`, now, id)
		}
		return err
	})
}

func (r *RequestRepository) TrialForTitle(ctx context.Context, serverID, userID string, tmdbID int) (*model.SeriesTrial, error) {
	var id int64
	var err = r.s.ro.QueryRowContext(ctx, `SELECT id FROM media_requests WHERE server_id=? AND user_id=? AND media_type='series' AND tmdb_id=?`, serverID, userID, tmdbID).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup trial request: %w", err)
	}
	return r.Trial(ctx, id)
}

// TrialEpisodeLimits returns the active trial episode limit of every listed series keyed by series ID; series without a trial are absent.
func (r *RequestRepository) TrialEpisodeLimits(ctx context.Context, seriesIDs []int64) (map[int64]int, error) {
	limits := make(map[int64]int, len(seriesIDs))
	for _, chunk := range chunkIDs(seriesIDs) {
		rows, err := r.s.ro.QueryContext(ctx, `SELECT mr.subject_id, MAX(MAX(t.episode_limit,1)) FROM series_trials t JOIN media_requests mr ON mr.id=t.request_id JOIN series sr ON sr.id=mr.subject_id AND sr.tmdb_id=mr.tmdb_id WHERE mr.subject_id IN (`+placeholders(len(chunk))+`) AND mr.subject_type='series' AND mr.cancelled_at IS NULL AND t.decision='' GROUP BY mr.subject_id`, int64Args(chunk)...)
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
			limits[pair[0]] = int(pair[1])
		}
	}
	return limits, nil
}
