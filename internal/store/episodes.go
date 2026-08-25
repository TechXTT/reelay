package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

type EpisodeRepository struct{ s *Store }

func (s *Store) Episodes() *EpisodeRepository { return &EpisodeRepository{s: s} }

func (r *EpisodeRepository) Create(ctx context.Context, in model.Episode, reason string) (model.Episode, error) {
	if in.SeriesID <= 0 || in.Season < 0 || in.Number < 0 {
		return model.Episode{}, errors.New("episode requires a series id and non-negative season/number")
	}
	if in.State == "" {
		in.State = model.StateUnmonitored
	}
	if !in.State.Valid() {
		return model.Episode{}, fmt.Errorf("invalid episode state %q", in.State)
	}
	if reason == "" {
		reason = "episode added"
	}
	if in.State == model.StateWanted && in.FirstWantedAt == nil {
		wantedAt := r.s.nowUTC()
		in.FirstWantedAt = &wantedAt
	}
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO episodes (
 series_id, season, number, absolute_number, title, air_date, state,
 chosen_release_id, imported_path, imported_quality, search_attempts,
 next_search_at, first_wanted_at, last_error
) VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?)`,
			in.SeriesID, in.Season, in.Number, in.AbsoluteNumber, in.Title,
			nullTime(in.AirDate), in.State, in.ChosenReleaseID, in.ImportedPath,
			in.ImportedQuality, in.SearchAttempts, nullTime(in.NextSearchAt),
			nullTime(in.FirstWantedAt), in.LastError)
		if err != nil {
			return fmt.Errorf("create episode S%02dE%02d: %w", in.Season, in.Number, err)
		}
		in.ID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		return insertTransition(ctx, tx, model.SubjectEpisode, in.ID, "", in.State, reason, "", r.s.nowUTC())
	})
	return in, err
}

func (r *EpisodeRepository) Get(ctx context.Context, id int64) (model.Episode, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectEpisodeSQL+" WHERE id = ?", id), scanEpisode, fmt.Sprintf("episode %d", id))
}

func (r *EpisodeRepository) BySeriesNumber(ctx context.Context, seriesID int64, season, number int) (model.Episode, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectEpisodeSQL+
		" WHERE series_id=? AND season=? AND number=?", seriesID, season, number), scanEpisode,
		fmt.Sprintf("series %d S%02dE%02d", seriesID, season, number))
}

func (r *EpisodeRepository) ListBySeries(ctx context.Context, seriesID int64) ([]model.Episode, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectEpisodeSQL+
		" WHERE series_id = ? ORDER BY season, number", seriesID)
	if err != nil {
		return nil, fmt.Errorf("list series %d episodes: %w", seriesID, err)
	}
	return collectRows(rows, scanEpisode)
}

func (r *EpisodeRepository) WantedDue(ctx context.Context, now time.Time, limit int) ([]model.Episode, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.s.ro.QueryContext(ctx, selectEpisodeSQL+`
 WHERE state='wanted' AND (next_search_at IS NULL OR next_search_at<=?)
 ORDER BY COALESCE(next_search_at, ''), id LIMIT ?`, FormatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list wanted episodes: %w", err)
	}
	return collectRows(rows, scanEpisode)
}

func (r *EpisodeRepository) SeriesHasActive(ctx context.Context, seriesID int64) (bool, error) {
	if seriesID <= 0 {
		return false, errors.New("active series check requires a series id")
	}
	var active bool
	err := r.s.ro.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM episodes WHERE series_id=? AND state IN ('grabbed','downloading','importing')
)`, seriesID).Scan(&active)
	if err != nil {
		return false, fmt.Errorf("check active series %d: %w", seriesID, err)
	}
	return active, nil
}

func (r *EpisodeRepository) ActiveByRelease(ctx context.Context, releaseID int64) ([]model.Episode, error) {
	if releaseID <= 0 {
		return nil, errors.New("active release lookup requires a release id")
	}
	rows, err := r.s.ro.QueryContext(ctx, selectEpisodeSQL+`
 WHERE chosen_release_id=? AND state IN ('grabbed','downloading','importing')
 ORDER BY series_id, season, number`, releaseID)
	if err != nil {
		return nil, fmt.Errorf("list active episodes for release %d: %w", releaseID, err)
	}
	return collectRows(rows, scanEpisode)
}

// UpsertMetadata refreshes provider-owned fields without overwriting lifecycle
// state. It returns created=true only when this episode was newly announced.
func (r *EpisodeRepository) UpsertMetadata(ctx context.Context, in model.Episode, initial model.ItemState, reason string) (model.Episode, bool, error) {
	if in.SeriesID <= 0 || in.Season < 0 || in.Number < 0 || !initial.Valid() {
		return model.Episode{}, false, errors.New("metadata episode has invalid identity or initial state")
	}
	if reason == "" {
		reason = "episode announced by metadata provider"
	}
	created := false
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var firstWanted any
		if initial == model.StateWanted {
			firstWanted = FormatTime(r.s.nowUTC())
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO episodes (
 series_id, season, number, absolute_number, title, air_date, state, first_wanted_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, in.SeriesID, in.Season, in.Number,
			in.AbsoluteNumber, in.Title, nullTime(in.AirDate), initial, firstWanted)
		if err != nil {
			return fmt.Errorf("upsert metadata episode: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			created = true
			in.ID, err = res.LastInsertId()
			if err != nil {
				return err
			}
			return insertTransition(ctx, tx, model.SubjectEpisode, in.ID, "", initial, reason, "", r.s.nowUTC())
		}
		_, err = tx.ExecContext(ctx, `UPDATE episodes SET absolute_number=?, title=?, air_date=?
 WHERE series_id=? AND season=? AND number=?`, in.AbsoluteNumber, in.Title,
			nullTime(in.AirDate), in.SeriesID, in.Season, in.Number)
		return err
	})
	if err != nil {
		return model.Episode{}, false, err
	}
	out, err := scanEpisode(r.s.ro.QueryRowContext(ctx, selectEpisodeSQL+
		" WHERE series_id=? AND season=? AND number=?", in.SeriesID, in.Season, in.Number))
	if err != nil {
		return model.Episode{}, false, fmt.Errorf("reload metadata episode: %w", err)
	}
	return out, created, nil
}

const selectEpisodeSQL = `SELECT id, series_id, season, number, absolute_number,
 title, air_date, state, COALESCE(chosen_release_id, 0), imported_path,
 imported_quality, search_attempts, next_search_at, first_wanted_at, last_error
 FROM episodes`

func scanEpisode(row scanner) (model.Episode, error) {
	var e model.Episode
	var air, next, first sql.NullString
	err := row.Scan(&e.ID, &e.SeriesID, &e.Season, &e.Number, &e.AbsoluteNumber,
		&e.Title, &air, &e.State, &e.ChosenReleaseID, &e.ImportedPath,
		&e.ImportedQuality, &e.SearchAttempts, &next, &first, &e.LastError)
	if err != nil {
		return e, err
	}
	var parseErr error
	if e.AirDate, parseErr = scanNullTime(air); parseErr != nil {
		return e, parseErr
	}
	if e.NextSearchAt, parseErr = scanNullTime(next); parseErr != nil {
		return e, parseErr
	}
	if e.FirstWantedAt, parseErr = scanNullTime(first); parseErr != nil {
		return e, parseErr
	}
	return e, nil
}
