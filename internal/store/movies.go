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

type MovieRepository struct{ s *Store }

func (s *Store) Movies() *MovieRepository { return &MovieRepository{s: s} }

func (r *MovieRepository) Create(ctx context.Context, in model.Movie, reason string) (model.Movie, error) {
	if err := requiredText("movie.title", in.Title); err != nil {
		return model.Movie{}, err
	}
	if err := requiredText("movie.root_folder", in.RootFolder); err != nil {
		return model.Movie{}, err
	}
	if in.ProfileID <= 0 || in.Year < 0 {
		return model.Movie{}, errors.New("movie requires a profile and a non-negative year")
	}
	if in.SortTitle == "" {
		in.SortTitle = strings.ToLower(in.Title)
	}
	if in.State == "" {
		in.State = model.StateWanted
	}
	if !in.State.Valid() {
		return model.Movie{}, fmt.Errorf("invalid movie state %q", in.State)
	}
	if reason == "" {
		reason = "movie added"
	}
	in.AddedAt = r.s.nowUTC()
	if in.State == model.StateWanted && in.FirstWantedAt == nil {
		wantedAt := in.AddedAt
		in.FirstWantedAt = &wantedAt
	}
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO movies (
 title, sort_title, year, tmdb_id, imdb_id, runtime_minutes,
 quality_profile_id, root_folder, state, chosen_release_id, imported_path,
 imported_quality, search_attempts, next_search_at, first_wanted_at,
 last_error, added_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, 0), ?, ?, ?, ?, ?, ?, ?)`,
			in.Title, in.SortTitle, in.Year, in.TMDBID, in.IMDBID, in.RuntimeMinutes,
			in.ProfileID, in.RootFolder, in.State, in.ChosenReleaseID, in.ImportedPath,
			in.ImportedQuality, in.SearchAttempts, nullTime(in.NextSearchAt),
			nullTime(in.FirstWantedAt), in.LastError, FormatTime(in.AddedAt))
		if err != nil {
			return fmt.Errorf("create movie %q: %w", in.Title, err)
		}
		in.ID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		return insertTransition(ctx, tx, model.SubjectMovie, in.ID, "", in.State, reason, "", in.AddedAt)
	})
	return in, err
}

func (r *MovieRepository) Get(ctx context.Context, id int64) (model.Movie, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectMovieSQL+" WHERE id = ?", id), scanMovie, fmt.Sprintf("movie %d", id))
}

func (r *MovieRepository) GetByTMDBID(ctx context.Context, id int) (model.Movie, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectMovieSQL+" WHERE tmdb_id = ?", id), scanMovie, fmt.Sprintf("movie tmdb %d", id))
}

func (r *MovieRepository) List(ctx context.Context) ([]model.Movie, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectMovieSQL+" ORDER BY sort_title, year")
	if err != nil {
		return nil, fmt.Errorf("list movies: %w", err)
	}
	return collectRows(rows, scanMovie)
}

func (r *MovieRepository) WantedDue(ctx context.Context, now time.Time, limit int) ([]model.Movie, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.s.ro.QueryContext(ctx, selectMovieSQL+`
 WHERE state='wanted' AND (next_search_at IS NULL OR next_search_at<=?)
 ORDER BY COALESCE(next_search_at, ''), id LIMIT ?`, FormatTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("list wanted movies: %w", err)
	}
	return collectRows(rows, scanMovie)
}

func (r *MovieRepository) Update(ctx context.Context, in model.Movie) (model.Movie, error) {
	if in.ID <= 0 || in.Title == "" || in.ProfileID <= 0 || in.RootFolder == "" || !in.State.Valid() {
		return in, errors.New("update movie: invalid movie")
	}
	res, err := r.s.rw.ExecContext(ctx, `UPDATE movies SET title=?, sort_title=?, year=?,
 tmdb_id=?, imdb_id=?, runtime_minutes=?, quality_profile_id=?, root_folder=? WHERE id=?`,
		in.Title, in.SortTitle, in.Year, in.TMDBID, in.IMDBID, in.RuntimeMinutes,
		in.ProfileID, in.RootFolder, in.ID)
	if err != nil {
		return in, fmt.Errorf("update movie %d: %w", in.ID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return in, fmt.Errorf("movie %d: %w", in.ID, ErrNotFound)
	}
	return r.Get(ctx, in.ID)
}

func (r *MovieRepository) Delete(ctx context.Context, id int64) error {
	return r.s.deleteOne(ctx, "DELETE FROM movies WHERE id=?", "movie", id)
}

const selectMovieSQL = `SELECT id, title, sort_title, year, tmdb_id, imdb_id,
 runtime_minutes, quality_profile_id, root_folder, state,
 COALESCE(chosen_release_id, 0), imported_path, imported_quality,
 search_attempts, next_search_at, first_wanted_at, last_error, added_at
 FROM movies`

func scanMovie(row scanner) (model.Movie, error) {
	var m model.Movie
	var next, first sql.NullString
	var added string
	err := row.Scan(&m.ID, &m.Title, &m.SortTitle, &m.Year, &m.TMDBID, &m.IMDBID,
		&m.RuntimeMinutes, &m.ProfileID, &m.RootFolder, &m.State,
		&m.ChosenReleaseID, &m.ImportedPath, &m.ImportedQuality, &m.SearchAttempts,
		&next, &first, &m.LastError, &added)
	if err != nil {
		return m, err
	}
	var parseErr error
	if m.NextSearchAt, parseErr = scanNullTime(next); parseErr != nil {
		return m, parseErr
	}
	if m.FirstWantedAt, parseErr = scanNullTime(first); parseErr != nil {
		return m, parseErr
	}
	m.AddedAt, parseErr = ParseTime(added)
	return m, parseErr
}
