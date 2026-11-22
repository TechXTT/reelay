package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/TechXTT/reelay/internal/model"
)

// A failed grab with a completed download is eligible only while the failed
// item still points to its release. Episode coverage is limited to the grab's
// series. One release must never have two active episode grabs.
const recoverableImport = `g.state='failed' AND g.progress=1 AND g.content_path<>''
 AND NOT EXISTS(SELECT 1 FROM grabs active WHERE active.subject_type=g.subject_type
   AND (active.subject_id=g.subject_id OR (g.subject_type='episode' AND active.release_id=g.release_id))
   AND active.state IN ('pending','downloading','completed','importing'))`

const recoverableMovieImport = `g.subject_type='movie' AND g.subject_id=m.id
 AND g.release_id=m.chosen_release_id AND m.state='import_failed' AND ` + recoverableImport

const recoverableEpisodeImport = `g.subject_type='episode' AND g.release_id=ep.chosen_release_id
 AND ep.state='import_failed' AND EXISTS(SELECT 1 FROM episodes anchor
  WHERE anchor.id=g.subject_id AND anchor.series_id=ep.series_id)
 AND NOT EXISTS(SELECT 1 FROM episodes active WHERE active.chosen_release_id=g.release_id
  AND active.state IN ('grabbed','downloading','importing'))
 AND ` + recoverableImport

// MovedByGrab returns video moves already recorded before a later import step failed.
func (r *ImportRepository) MovedByGrab(ctx context.Context, grabID int64) ([]model.ImportRecord, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT subject_id,source_path,dest_path,size_bytes FROM imports
 WHERE grab_id=? AND method='move' ORDER BY id`, grabID)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(row scanner) (model.ImportRecord, error) {
		var record model.ImportRecord

		err := row.Scan(&record.SubjectID, &record.SourcePath, &record.DestPath, &record.SizeBytes)
		return record, err
	})
}

// ImportOutcomes counts the recorded outcomes for a shared episode grab,
// including coverage outside the request that may have retried part of it.
func (r *EpisodeRepository) ImportOutcomes(ctx context.Context, grab model.Grab) (imported, failed bool, err error) {
	err = r.s.ro.QueryRowContext(ctx, `SELECT
 COALESCE(MAX(ep.state='imported'),0), COALESCE(MAX(ep.state='import_failed'),0)
 FROM episodes ep JOIN episodes anchor ON anchor.id=? AND anchor.series_id=ep.series_id
 WHERE ep.chosen_release_id=? AND ep.state IN ('imported','import_failed')`, grab.SubjectID, grab.ReleaseID).
		Scan(&imported, &failed)
	return
}

// FailImport preserves successes from a partial pack and records the remaining
// failures with the grab so a restart cannot strand an item in importing.
func (r *GrabRepository) FailImport(ctx context.Context, grab model.Grab, subjectIDs []int64, detail string) error {
	var now = r.s.nowUTC()

	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE grabs SET state='failed', last_error=?, updated_at=?
 WHERE id=? AND state='importing'`, detail, FormatTime(now), grab.ID)
		if err != nil {
			return err
		}
		if count, err := res.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return ErrConflict
		}
		for _, id := range subjectIDs {
			res, err := tx.ExecContext(ctx, `UPDATE `+itemTable(grab.SubjectType)+` SET state='import_failed', last_error=?
 WHERE id=? AND state='importing'`, detail, id)
			if err != nil {
				return err
			}
			count, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if count == 0 {
				continue
			}
			if err := insertTransition(ctx, tx, grab.SubjectType, id, model.StateImporting,
				model.StateImportFailed, "import failed", detail, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// RetryImports puts the failed subjects in this request back into importing
// and requeues their original grabs in one transaction. Other episodes in a
// shared pack keep their state, including episodes already imported.
func (r *RequestRepository) RetryImports(ctx context.Context, request model.MediaRequest) (int, error) {
	var retried int
	var subject = model.SubjectMovie

	if request.MediaType == "series" {
		subject = model.SubjectEpisode
	}
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		type candidate struct {
			subjectID int64
			grabID    int64
		}
		var candidates []candidate

		if subject == model.SubjectMovie {
			var retry candidate
			err := tx.QueryRowContext(ctx, `SELECT m.id,g.id FROM movies m JOIN grabs g ON `+recoverableMovieImport+
				` WHERE m.id=? AND m.tmdb_id=? ORDER BY g.id DESC LIMIT 1`, request.SubjectID, request.TMDBID).
				Scan(&retry.subjectID, &retry.grabID)
			if err == sql.ErrNoRows {
				return nil
			}
			if err != nil {
				return err
			}
			candidates = append(candidates, retry)
		} else {
			rows, err := tx.QueryContext(ctx, `SELECT ep.id,ep.season,
 (SELECT g.id FROM grabs g WHERE `+recoverableEpisodeImport+` ORDER BY g.id DESC LIMIT 1)
 FROM episodes ep JOIN series sr ON sr.id=ep.series_id
 WHERE ep.series_id=? AND sr.tmdb_id=? AND sr.status='following' AND ep.state='import_failed'`,
				request.SubjectID, request.TMDBID)
			if err != nil {
				return err
			}
			candidates, err = collectRows(rows, func(row scanner) (candidate, error) {
				var episode model.Episode
				var grabID sql.NullInt64

				if err := row.Scan(&episode.ID, &episode.Season, &grabID); err != nil {
					return candidate{}, err
				}
				if !grabID.Valid || !request.IncludesEpisode(episode) {
					return candidate{}, nil
				}
				return candidate{subjectID: episode.ID, grabID: grabID.Int64}, nil
			})
			if err != nil {
				return err
			}
		}

		var grabs = make(map[int64]bool)
		var now = r.s.nowUTC()

		for _, retry := range candidates {
			if retry.grabID == 0 {
				continue
			}
			if !grabs[retry.grabID] {
				if _, err := tx.ExecContext(ctx, `UPDATE grabs SET state='completed', last_error='', updated_at=? WHERE id=?`,
					FormatTime(now), retry.grabID); err != nil {
					return err
				}
				grabs[retry.grabID] = true
			}
			if _, err := tx.ExecContext(ctx, `UPDATE `+itemTable(subject)+` SET state='importing',
 next_search_at=NULL,last_error='' WHERE id=? AND state='import_failed'`, retry.subjectID); err != nil {
				return err
			}
			if err := insertTransition(ctx, tx, subject, retry.subjectID, model.StateImportFailed,
				model.StateImporting, "import retry scheduled", fmt.Sprintf("grab_id=%d", retry.grabID), now); err != nil {
				return err
			}
			retried++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("queue import retries: %w", err)
	}
	return retried, nil
}
