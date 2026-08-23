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

type SeriesRepository struct{ s *Store }

func (s *Store) Series() *SeriesRepository { return &SeriesRepository{s: s} }

func (r *SeriesRepository) Create(ctx context.Context, in model.Series) (model.Series, error) {
	if err := validateSeries(in); err != nil {
		return model.Series{}, err
	}
	aliases, err := encodeJSON(in.Aliases)
	if err != nil {
		return model.Series{}, err
	}
	if in.SortTitle == "" {
		in.SortTitle = strings.ToLower(in.Title)
	}
	if in.MonitorMode == "" {
		in.MonitorMode = model.MonitorFutureOnly
	}
	if in.Status == "" {
		in.Status = model.SeriesFollowing
	}
	in.AddedAt = r.s.nowUTC()
	res, err := r.s.rw.ExecContext(ctx, `INSERT INTO series (
 title, sort_title, year, aliases_json, tvmaze_id, tmdb_id, imdb_id, is_anime,
 absolute_offset, monitor_mode, quality_profile_id, root_folder, status,
 runtime_minutes, added_at, episodes_refreshed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Title, in.SortTitle, in.Year, aliases, in.TVmazeID, in.TMDBID, in.IMDBID,
		in.IsAnime, in.AbsoluteOffset, in.MonitorMode, in.ProfileID, in.RootFolder,
		in.Status, in.RuntimeMinutes, FormatTime(in.AddedAt), nullTime(&in.EpisodesRefreshedAt))
	if err != nil {
		return model.Series{}, fmt.Errorf("create series %q: %w", in.Title, err)
	}
	in.ID, err = res.LastInsertId()
	if err != nil {
		return model.Series{}, fmt.Errorf("create series id: %w", err)
	}
	return in, nil
}

func (r *SeriesRepository) Get(ctx context.Context, id int64) (model.Series, error) {
	s, err := scanSeries(r.s.ro.QueryRowContext(ctx, selectSeriesSQL+" WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return s, fmt.Errorf("series %d: %w", id, ErrNotFound)
	}
	return s, err
}

func (r *SeriesRepository) List(ctx context.Context) ([]model.Series, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectSeriesSQL+" ORDER BY sort_title, year")
	if err != nil {
		return nil, fmt.Errorf("list series: %w", err)
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		v, err := scanSeries(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *SeriesRepository) ListFollowing(ctx context.Context, limit int) ([]model.Series, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := r.s.ro.QueryContext(ctx, selectSeriesSQL+
		" WHERE status = 'following' ORDER BY COALESCE(episodes_refreshed_at, '') LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("list followed series: %w", err)
	}
	defer rows.Close()
	var out []model.Series
	for rows.Next() {
		v, err := scanSeries(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *SeriesRepository) Update(ctx context.Context, in model.Series) (model.Series, error) {
	if in.ID <= 0 || validateSeries(in) != nil {
		return in, errors.New("update series: invalid series")
	}
	aliases, err := encodeJSON(in.Aliases)
	if err != nil {
		return in, err
	}
	res, err := r.s.rw.ExecContext(ctx, `UPDATE series SET title=?, sort_title=?, year=?,
 aliases_json=?, tvmaze_id=?, tmdb_id=?, imdb_id=?, is_anime=?, absolute_offset=?,
 monitor_mode=?, quality_profile_id=?, root_folder=?, status=?, runtime_minutes=? WHERE id=?`,
		in.Title, in.SortTitle, in.Year, aliases, in.TVmazeID, in.TMDBID, in.IMDBID,
		in.IsAnime, in.AbsoluteOffset, in.MonitorMode, in.ProfileID, in.RootFolder,
		in.Status, in.RuntimeMinutes, in.ID)
	if err != nil {
		return in, fmt.Errorf("update series %d: %w", in.ID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return in, fmt.Errorf("series %d: %w", in.ID, ErrNotFound)
	}
	return r.Get(ctx, in.ID)
}

func (r *SeriesRepository) Delete(ctx context.Context, id int64) error {
	res, err := r.s.rw.ExecContext(ctx, "DELETE FROM series WHERE id=?", id)
	if err != nil {
		return fmt.Errorf("delete series %d: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("series %d: %w", id, ErrNotFound)
	}
	return nil
}

func (r *SeriesRepository) MarkRefreshed(ctx context.Context, id int64, at time.Time) error {
	res, err := r.s.rw.ExecContext(ctx, "UPDATE series SET episodes_refreshed_at=? WHERE id=?",
		FormatTime(at), id)
	if err != nil {
		return fmt.Errorf("mark series %d refreshed: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("series %d: %w", id, ErrNotFound)
	}
	return nil
}

func validateSeries(s model.Series) error {
	if err := requiredText("series.title", s.Title); err != nil {
		return err
	}
	if err := requiredText("series.root_folder", s.RootFolder); err != nil {
		return err
	}
	if s.ProfileID <= 0 {
		return errors.New("series.quality_profile_id must be positive")
	}
	if s.MonitorMode != "" && !s.MonitorMode.Valid() {
		return fmt.Errorf("invalid monitor mode %q", s.MonitorMode)
	}
	if s.Status != "" && !s.Status.Valid() {
		return fmt.Errorf("invalid series status %q", s.Status)
	}
	return nil
}

const selectSeriesSQL = `SELECT id, title, sort_title, year, aliases_json,
 tvmaze_id, tmdb_id, imdb_id, is_anime, absolute_offset, monitor_mode,
 quality_profile_id, root_folder, status, runtime_minutes, added_at,
 episodes_refreshed_at FROM series`

func scanSeries(row scanner) (model.Series, error) {
	var s model.Series
	var aliases, added string
	var anime int
	var refreshed sql.NullString
	err := row.Scan(&s.ID, &s.Title, &s.SortTitle, &s.Year, &aliases,
		&s.TVmazeID, &s.TMDBID, &s.IMDBID, &anime, &s.AbsoluteOffset,
		&s.MonitorMode, &s.ProfileID, &s.RootFolder, &s.Status, &s.RuntimeMinutes,
		&added, &refreshed)
	if err != nil {
		return s, err
	}
	s.IsAnime = anime == 1
	if err := decodeJSON(aliases, &s.Aliases); err != nil {
		return s, fmt.Errorf("scan series %d aliases: %w", s.ID, err)
	}
	s.AddedAt, err = ParseTime(added)
	if err != nil {
		return s, err
	}
	if t, err := scanNullTime(refreshed); err != nil {
		return s, err
	} else if t != nil {
		s.EpisodesRefreshedAt = *t
	}
	return s, nil
}
