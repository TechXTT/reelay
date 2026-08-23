package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/TechXTT/reelay/internal/model"
)

type DecisionRepository struct{ s *Store }

func (s *Store) Decisions() *DecisionRepository { return &DecisionRepository{s: s} }

// ReplaceCandidates stores the latest complete evaluation set for an item.
// Delete and insert share a transaction, so the UI never sees half a search.
func (r *DecisionRepository) ReplaceCandidates(ctx context.Context, subject model.SubjectType, id int64, values []model.CandidateEvaluation) error {
	if !subject.ValidItem() || id <= 0 {
		return fmt.Errorf("replace candidates: invalid subject %q:%d", subject, id)
	}
	now := r.s.nowUTC()
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := itemState(ctx, tx, subject, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM candidate_evaluations
 WHERE subject_type=? AND subject_id=?`, subject, id); err != nil {
			return fmt.Errorf("clear candidate evaluations: %w", err)
		}
		for _, v := range values {
			if v.ReleaseID <= 0 {
				return errors.New("candidate evaluation requires a release id")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO candidate_evaluations
 (subject_type, subject_id, release_id, accepted, reason_code, reason, score, evaluated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, subject, id, v.ReleaseID, v.Accepted,
				v.ReasonCode, v.Reason, v.Score, FormatTime(now)); err != nil {
				return fmt.Errorf("insert candidate evaluation: %w", err)
			}
		}
		return nil
	})
}

func (r *DecisionRepository) Candidates(ctx context.Context, subject model.SubjectType, id int64) ([]model.CandidateEvaluation, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT id, release_id, accepted,
 reason_code, reason, score, evaluated_at FROM candidate_evaluations
 WHERE subject_type=? AND subject_id=? ORDER BY accepted DESC, score DESC, id`, subject, id)
	if err != nil {
		return nil, fmt.Errorf("list candidates: %w", err)
	}
	defer rows.Close()
	var out []model.CandidateEvaluation
	for rows.Next() {
		var v model.CandidateEvaluation
		var accepted int
		var at string
		if err := rows.Scan(&v.ID, &v.ReleaseID, &accepted, &v.ReasonCode,
			&v.Reason, &v.Score, &at); err != nil {
			return nil, err
		}
		v.SubjectType, v.SubjectID, v.Accepted = subject, id, accepted == 1
		v.EvaluatedAt, err = ParseTime(at)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *DecisionRepository) Blacklist(ctx context.Context, subject model.SubjectType, id int64, hash, reason string) error {
	if !subject.ValidItem() || id <= 0 || strings.TrimSpace(hash) == "" {
		return errors.New("blacklist requires a valid subject and info hash")
	}
	if reason == "" {
		reason = "failed grab"
	}
	_, err := r.s.rw.ExecContext(ctx, `INSERT INTO release_blacklist
 (subject_type, subject_id, info_hash, reason, created_at) VALUES (?, ?, ?, ?, ?)
 ON CONFLICT(subject_type, subject_id, info_hash) DO UPDATE SET
 reason=excluded.reason, created_at=excluded.created_at`, subject, id,
		strings.ToLower(strings.TrimSpace(hash)), reason, FormatTime(r.s.nowUTC()))
	if err != nil {
		return fmt.Errorf("blacklist %s:%d release: %w", subject, id, err)
	}
	return nil
}

func (r *DecisionRepository) IsBlacklisted(ctx context.Context, subject model.SubjectType, id int64, hash string) (bool, error) {
	var one int
	err := r.s.ro.QueryRowContext(ctx, `SELECT 1 FROM release_blacklist
 WHERE subject_type=? AND subject_id=? AND info_hash=?`, subject, id,
		strings.ToLower(strings.TrimSpace(hash))).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check blacklist: %w", err)
	}
	return true, nil
}

func (r *DecisionRepository) BlacklistFor(ctx context.Context, subject model.SubjectType, id int64) (map[string]bool, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT info_hash FROM release_blacklist
 WHERE subject_type=? AND subject_id=?`, subject, id)
	if err != nil {
		return nil, fmt.Errorf("list blacklist: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out[strings.ToLower(hash)] = true
	}
	return out, rows.Err()
}

type ImportRepository struct{ s *Store }

func (s *Store) Imports() *ImportRepository { return &ImportRepository{s: s} }

func (r *ImportRepository) Create(ctx context.Context, in model.ImportRecord) (model.ImportRecord, error) {
	if in.GrabID <= 0 || !in.SubjectType.ValidItem() || in.SubjectID <= 0 {
		return in, errors.New("import requires a grab and valid subject")
	}
	switch in.Method {
	case "hardlink", "copy", "move":
	default:
		return in, fmt.Errorf("invalid import method %q", in.Method)
	}
	if err := requiredText("import.dest_path", in.DestPath); err != nil {
		return in, err
	}
	in.At = r.s.nowUTC()
	res, err := r.s.rw.ExecContext(ctx, `INSERT INTO imports (
 grab_id, subject_type, subject_id, source_path, dest_path, method,
 size_bytes, replaced_path, imported_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.GrabID, in.SubjectType, in.SubjectID, in.SourcePath, in.DestPath,
		in.Method, in.SizeBytes, in.ReplacedPath, FormatTime(in.At))
	if err != nil {
		return in, fmt.Errorf("record import: %w", err)
	}
	in.ID, err = res.LastInsertId()
	return in, err
}
