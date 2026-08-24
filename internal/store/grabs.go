package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

type GrabRepository struct{ s *Store }

func (s *Store) Grabs() *GrabRepository { return &GrabRepository{s: s} }

func (r *GrabRepository) Create(ctx context.Context, in model.Grab) (model.Grab, error) {
	if !in.SubjectType.ValidItem() || in.SubjectID <= 0 || in.ReleaseID <= 0 {
		return in, errors.New("grab requires a valid subject and release")
	}
	if err := requiredText("grab.torrent_hash", in.TorrentHash); err != nil {
		return in, err
	}
	if err := requiredText("grab.category", in.Category); err != nil {
		return in, err
	}
	if in.State == "" {
		in.State = model.GrabPending
	}
	if !in.State.Valid() {
		return in, fmt.Errorf("invalid grab state %q", in.State)
	}
	in.TorrentHash = strings.ToLower(in.TorrentHash)
	in.CreatedAt = r.s.nowUTC()
	in.UpdatedAt = in.CreatedAt
	if in.ProgressedAt.IsZero() {
		in.ProgressedAt = in.CreatedAt
	}
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := itemState(ctx, tx, in.SubjectType, in.SubjectID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO grabs (
 subject_type, subject_id, release_id, torrent_hash, category, state, progress,
 content_path, attempts, last_error, created_at, updated_at, progressed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.SubjectType, in.SubjectID,
			in.ReleaseID, in.TorrentHash, in.Category, in.State, in.Progress,
			in.ContentPath, in.Attempts, in.LastError, FormatTime(in.CreatedAt),
			FormatTime(in.UpdatedAt), FormatTime(in.ProgressedAt))
		if err != nil {
			return fmt.Errorf("create grab: %w", err)
		}
		in.ID, err = res.LastInsertId()
		return err
	})
	return in, err
}

// CreateGrabbed records the client handoff and advances the item in one
// transaction. A torrent must never exist in the database without its item
// pointing at the same chosen release.
func (r *GrabRepository) CreateGrabbed(ctx context.Context, lock *ItemLock, in model.Grab, reason string) (model.Grab, error) {
	if lock == nil || in.SubjectType != lock.Subject || in.SubjectID != lock.ID {
		return in, errors.New("create grabbed requires the matching item lock")
	}
	if in.ReleaseID <= 0 || in.TorrentHash == "" || in.Category == "" {
		return in, errors.New("create grabbed requires release, hash, and category")
	}
	in.State = model.GrabPending
	in.TorrentHash = strings.ToLower(in.TorrentHash)
	in.CreatedAt = r.s.nowUTC()
	in.UpdatedAt, in.ProgressedAt = in.CreatedAt, in.CreatedAt
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		from, err := itemState(ctx, tx, lock.Subject, lock.ID)
		if err != nil {
			return err
		}
		if from != model.StateSearching {
			return fmt.Errorf("create grabbed from %s: %w", from, ErrInvalidTransition)
		}
		var activeID int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM grabs
 WHERE subject_type=? AND subject_id=?
 AND state IN ('pending','downloading','completed','importing') LIMIT 1`,
			in.SubjectType, in.SubjectID).Scan(&activeID)
		if err == nil {
			return fmt.Errorf("%s:%d already has active grab %d: %w",
				in.SubjectType, in.SubjectID, activeID, ErrItemBusy)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check active grab: %w", err)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO grabs (
 subject_type, subject_id, release_id, torrent_hash, category, state, progress,
 content_path, attempts, last_error, created_at, updated_at, progressed_at
) VALUES (?, ?, ?, ?, ?, ?, 0, '', 0, '', ?, ?, ?)`, in.SubjectType, in.SubjectID,
			in.ReleaseID, in.TorrentHash, in.Category, in.State, FormatTime(in.CreatedAt),
			FormatTime(in.UpdatedAt), FormatTime(in.ProgressedAt))
		if err != nil {
			return fmt.Errorf("insert grab: %w", err)
		}
		in.ID, err = res.LastInsertId()
		if err != nil {
			return err
		}
		table := "episodes"
		if lock.Subject == model.SubjectMovie {
			table = "movies"
		}
		res, err = tx.ExecContext(ctx, `UPDATE `+table+` SET state='grabbed',
 chosen_release_id=?, next_search_at=NULL, last_error='' WHERE id=? AND state=?`,
			in.ReleaseID, lock.ID, from)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		return insertTransition(ctx, tx, lock.Subject, lock.ID, from,
			model.StateGrabbed, reason, fmt.Sprintf("release_id=%d grab_id=%d", in.ReleaseID, in.ID), in.CreatedAt)
	})
	return in, err
}

func (r *GrabRepository) Update(ctx context.Context, in model.Grab) error {
	if in.ID <= 0 || !in.State.Valid() || in.Progress < 0 || in.Progress > 1 {
		return errors.New("update grab: invalid id, state, or progress")
	}
	in.UpdatedAt = r.s.nowUTC()
	res, err := r.s.rw.ExecContext(ctx, `UPDATE grabs SET state=?, progress=?,
 content_path=?, attempts=?, last_error=?, updated_at=?, progressed_at=? WHERE id=?`,
		in.State, in.Progress, in.ContentPath, in.Attempts, in.LastError,
		FormatTime(in.UpdatedAt), nullTime(&in.ProgressedAt), in.ID)
	if err != nil {
		return fmt.Errorf("update grab %d: %w", in.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("grab %d: %w", in.ID, ErrNotFound)
	}
	return nil
}

func (r *GrabRepository) Get(ctx context.Context, id int64) (model.Grab, error) {
	return findOne(r.s.ro.QueryRowContext(ctx, selectGrabSQL+" WHERE id=?", id), scanGrab, fmt.Sprintf("grab %d", id))
}

func (r *GrabRepository) Active(ctx context.Context) ([]model.Grab, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectGrabSQL+
		" WHERE state IN ('pending','downloading','completed','importing') ORDER BY created_at")
	if err != nil {
		return nil, fmt.Errorf("list active grabs: %w", err)
	}
	return collectRows(rows, scanGrab)
}

func (r *GrabRepository) History(ctx context.Context, limit, offset int) ([]model.Grab, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := r.s.ro.QueryContext(ctx, selectGrabSQL+
		" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, fmt.Errorf("grab history: %w", err)
	}
	return collectRows(rows, scanGrab)
}

func (r *GrabRepository) BySubject(ctx context.Context, subject model.SubjectType, id int64) ([]model.Grab, error) {
	if !subject.ValidItem() || id <= 0 {
		return nil, errors.New("list subject grabs: invalid subject or id")
	}
	rows, err := r.s.ro.QueryContext(ctx, selectGrabSQL+
		" WHERE subject_type=? AND subject_id=? ORDER BY created_at DESC, id DESC", subject, id)
	if err != nil {
		return nil, fmt.Errorf("list grabs for %s:%d: %w", subject, id, err)
	}
	return collectRows(rows, scanGrab)
}

const selectGrabSQL = `SELECT id, subject_type, subject_id, release_id,
 torrent_hash, category, state, progress, content_path, attempts, last_error,
 created_at, updated_at, progressed_at FROM grabs`

func scanGrab(row scanner) (model.Grab, error) {
	var v model.Grab
	var created, updated string
	var progressed sql.NullString
	err := row.Scan(&v.ID, &v.SubjectType, &v.SubjectID, &v.ReleaseID,
		&v.TorrentHash, &v.Category, &v.State, &v.Progress, &v.ContentPath,
		&v.Attempts, &v.LastError, &created, &updated, &progressed)
	if err != nil {
		return v, err
	}
	v.CreatedAt, err = ParseTime(created)
	if err != nil {
		return v, err
	}
	v.UpdatedAt, err = ParseTime(updated)
	if err != nil {
		return v, err
	}
	if t, err := scanNullTime(progressed); err != nil {
		return v, err
	} else if t != nil {
		v.ProgressedAt = *t
	}
	return v, nil
}
