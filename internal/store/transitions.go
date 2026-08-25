package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

type TransitionRepository struct{ s *Store }

func (s *Store) Transitions() *TransitionRepository { return &TransitionRepository{s: s} }

func (r *TransitionRepository) Transition(ctx context.Context, subject model.SubjectType, id int64, to model.ItemState, reason, detail string) (model.StateTransition, error) {
	if !subject.ValidItem() || id <= 0 {
		return model.StateTransition{}, fmt.Errorf("transition: invalid subject %q:%d", subject, id)
	}
	lock, err := r.s.Locks().Acquire(ctx, subject, id, "", 30*time.Second)
	if err != nil {
		return model.StateTransition{}, err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	return r.TransitionLocked(ctx, lock, to, reason, detail)
}

func (r *TransitionRepository) TransitionLocked(ctx context.Context, lock *ItemLock, to model.ItemState, reason, detail string) (model.StateTransition, error) {
	var out model.StateTransition
	if lock == nil || !lock.Subject.ValidItem() || lock.ID <= 0 {
		return out, errors.New("transition requires an item lock")
	}
	if !to.Valid() {
		return out, fmt.Errorf("transition: invalid target state %q", to)
	}
	if err := requiredText("transition.reason", reason); err != nil {
		return out, err
	}

	now := r.s.nowUTC()
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		from, err := itemState(ctx, tx, lock.Subject, lock.ID)
		if err != nil {
			return err
		}
		if !from.CanTransitionTo(to) {
			return fmt.Errorf("%s:%d %s -> %s: %w", lock.Subject, lock.ID, from, to, ErrInvalidTransition)
		}
		res, err := updateItemState(ctx, tx, lock.Subject, lock.ID, from, to, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("%s:%d changed concurrently: %w", lock.Subject, lock.ID, ErrConflict)
		}
		if err := insertTransition(ctx, tx, lock.Subject, lock.ID, from, to, reason, detail, now); err != nil {
			return err
		}
		out = model.StateTransition{
			SubjectType: lock.Subject, SubjectID: lock.ID, From: from, To: to,
			Reason: reason, Detail: detail, At: now,
		}
		return nil
	})
	return out, err
}

func (r *TransitionRepository) SearchRetryLocked(ctx context.Context, lock *ItemLock, next time.Time, reason, detail string, terminal bool) (model.StateTransition, error) {
	if lock == nil {
		return model.StateTransition{}, errors.New("search retry requires an item lock")
	}
	now := r.s.nowUTC()
	var out model.StateTransition
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		table := "episodes"
		if lock.Subject == model.SubjectMovie {
			table = "movies"
		}
		var from model.ItemState
		var importedPath string
		err := tx.QueryRowContext(ctx, `SELECT state, imported_path FROM `+table+` WHERE id=?`,
			lock.ID).Scan(&from, &importedPath)
		if err != nil {
			return err
		}
		to := model.StateWanted
		if importedPath != "" {
			// A failed upgrade search does not invalidate the existing import.
			to = model.StateImported
		} else if terminal {
			to = model.StateFailed
		}
		validUpgradeFallback := to == model.StateImported && importedPath != ""
		if from != model.StateSearching || (!from.CanTransitionTo(to) && !validUpgradeFallback) {
			return fmt.Errorf("%s:%d %s -> %s: %w", lock.Subject, lock.ID, from, to, ErrInvalidTransition)
		}
		var nextValue any = nullTime(&next)
		if terminal || to == model.StateImported {
			nextValue = nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state=?,
 search_attempts=search_attempts+1, next_search_at=?, last_error=?
	WHERE id=? AND state=?`, to, nextValue, detail, lock.ID, from)
		if err != nil {
			return fmt.Errorf("schedule retry: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		if err := insertTransition(ctx, tx, lock.Subject, lock.ID, from, to, reason, detail, now); err != nil {
			return err
		}
		out = model.StateTransition{SubjectType: lock.Subject, SubjectID: lock.ID,
			From: from, To: to, Reason: reason, Detail: detail, At: now}
		return nil
	})
	return out, err
}

// MarkImportedLocked records the library path and lifecycle transition in one
// transaction, so a file cannot be visible on disk while the item remains
// indefinitely stuck in importing after a process restart.
func (r *TransitionRepository) MarkImportedLocked(ctx context.Context, lock *ItemLock, path, quality, reason string) error {
	if lock == nil || path == "" || reason == "" {
		return errors.New("mark imported requires an item lock, path, and reason")
	}
	now := r.s.nowUTC()
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		from, err := itemState(ctx, tx, lock.Subject, lock.ID)
		if err != nil {
			return err
		}
		if from != model.StateImporting {
			return fmt.Errorf("%s:%d %s -> imported: %w", lock.Subject, lock.ID, from, ErrInvalidTransition)
		}
		table := "episodes"
		if lock.Subject == model.SubjectMovie {
			table = "movies"
		}
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state='imported',
 imported_path=?, imported_quality=?, last_error='', next_search_at=NULL
 WHERE id=? AND state='importing'`, path, quality, lock.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		return insertTransition(ctx, tx, lock.Subject, lock.ID, from,
			model.StateImported, reason, path, now)
	})
}

func (r *TransitionRepository) MarkImported(ctx context.Context, subject model.SubjectType, id int64, path, quality, reason string) error {
	lock, err := r.s.Locks().Acquire(ctx, subject, id, "mark-imported", 10*time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	return r.MarkImportedLocked(ctx, lock, path, quality, reason)
}

func (r *TransitionRepository) RetryNow(ctx context.Context, subject model.SubjectType, id int64, reason string) error {
	return r.retryNow(ctx, subject, id, reason, true)
}

// RequestSearchNow schedules immediate work without abandoning an existing
// search, download, or import. Queue removal uses RetryNow after explicitly
// stopping the torrent; user-facing search actions must use this stricter path.
func (r *TransitionRepository) RequestSearchNow(ctx context.Context, subject model.SubjectType, id int64, reason string) error {
	return r.retryNow(ctx, subject, id, reason, false)
}

func (r *TransitionRepository) retryNow(ctx context.Context, subject model.SubjectType, id int64, reason string, allowBusy bool) error {
	lock, err := r.s.Locks().Acquire(ctx, subject, id, "manual-retry", time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		from, err := itemState(ctx, tx, subject, id)
		if err != nil {
			return err
		}
		if !allowBusy && (from == model.StateSearching || from.Active()) {
			return fmt.Errorf("%s:%d is %s: %w", subject, id, from, ErrItemBusy)
		}
		if from != model.StateWanted {
			if !from.CanTransitionTo(model.StateWanted) {
				return fmt.Errorf("%s:%d %s -> wanted: %w", subject, id, from, ErrInvalidTransition)
			}
		}
		table := "episodes"
		if subject == model.SubjectMovie {
			table = "movies"
		}
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET state='wanted',
 next_search_at=NULL, search_attempts=0, last_error='', first_wanted_at=COALESCE(first_wanted_at, ?)
 WHERE id=? AND state=?`, FormatTime(r.s.nowUTC()), id, from)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		return insertTransition(ctx, tx, subject, id, from, model.StateWanted,
			reason, "retry scheduled immediately", r.s.nowUTC())
	})
}

func (r *TransitionRepository) History(ctx context.Context, subject model.SubjectType, id int64, limit int) ([]model.StateTransition, error) {
	if !subject.ValidItem() || id <= 0 {
		return nil, fmt.Errorf("history: invalid subject %q:%d", subject, id)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := r.s.ro.QueryContext(ctx, `SELECT id, from_state, to_state,
 reason, detail, transitioned_at FROM state_transitions
 WHERE subject_type = ? AND subject_id = ?
 ORDER BY transitioned_at DESC, id DESC LIMIT ?`, subject, id, limit)
	if err != nil {
		return nil, fmt.Errorf("transition history %s:%d: %w", subject, id, err)
	}
	defer rows.Close()
	var out []model.StateTransition
	for rows.Next() {
		var v model.StateTransition
		var from sql.NullString
		var at string
		if err := rows.Scan(&v.ID, &from, &v.To, &v.Reason, &v.Detail, &at); err != nil {
			return nil, err
		}
		v.SubjectType, v.SubjectID = subject, id
		if from.Valid {
			v.From = model.ItemState(from.String)
		}
		v.At, err = ParseTime(at)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func itemState(ctx context.Context, tx *sql.Tx, subject model.SubjectType, id int64) (model.ItemState, error) {
	table := "episodes"
	if subject == model.SubjectMovie {
		table = "movies"
	}
	var state model.ItemState
	err := tx.QueryRowContext(ctx, "SELECT state FROM "+table+" WHERE id = ?", id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%s:%d: %w", subject, id, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("read %s:%d state: %w", subject, id, err)
	}
	return state, nil
}

func updateItemState(ctx context.Context, tx *sql.Tx, subject model.SubjectType, id int64, from, to model.ItemState, now time.Time) (sql.Result, error) {
	table := "episodes"
	if subject == model.SubjectMovie {
		table = "movies"
	}
	q := `UPDATE ` + table + ` SET state = ?,
 first_wanted_at = CASE WHEN ? = 'wanted' THEN COALESCE(first_wanted_at, ?) ELSE first_wanted_at END,
 next_search_at = CASE WHEN ? IN ('searching','grabbed','downloading','importing','imported') THEN NULL ELSE next_search_at END
 WHERE id = ? AND state = ?`
	res, err := tx.ExecContext(ctx, q, to, to, FormatTime(now), to, id, from)
	if err != nil {
		return nil, fmt.Errorf("update %s:%d state: %w", subject, id, err)
	}
	return res, nil
}

func insertTransition(ctx context.Context, tx *sql.Tx, subject model.SubjectType, id int64, from, to model.ItemState, reason, detail string, at time.Time) error {
	var fromValue any
	if from != "" {
		fromValue = from
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO state_transitions
 (subject_type, subject_id, from_state, to_state, reason, detail, transitioned_at)
 VALUES (?, ?, ?, ?, ?, ?, ?)`, subject, id, fromValue, to, reason, detail, FormatTime(at))
	if err != nil {
		return fmt.Errorf("audit %s:%d transition: %w", subject, id, err)
	}
	return nil
}
