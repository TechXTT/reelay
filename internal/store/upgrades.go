package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

// upgradeBatchLimit caps how many imported items one upgrade query returns.
const upgradeBatchLimit = 500

// inflightUpgradeStates are the states an item with an imported file passes
// through while an upgrade search or download is running.
const inflightUpgradeStates = `('wanted','searching','grabbed','downloading','importing')`

// InflightUpgrades counts movies that already have an imported file and are
// being searched for, downloaded or imported again.
func (r *MovieRepository) InflightUpgrades(ctx context.Context) (int, error) {
	return inflightUpgrades(ctx, r.s.ro, "movies")
}

// InflightUpgrades counts episodes that already have an imported file and are
// being searched for, downloaded or imported again.
func (r *EpisodeRepository) InflightUpgrades(ctx context.Context) (int, error) {
	return inflightUpgrades(ctx, r.s.ro, "episodes")
}

// UpgradeDue lists imported movies never checked for an upgrade or last
// checked at or before checkedBefore, never-checked first, then oldest check.
func (r *MovieRepository) UpgradeDue(ctx context.Context, checkedBefore time.Time, limit int) ([]model.Movie, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectMovieSQL+upgradeDueSQL, FormatTime(checkedBefore), clampUpgradeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list movies due for upgrade check: %w", err)
	}
	return collectRows(rows, scanMovie)
}

// UpgradeDue lists imported episodes never checked for an upgrade or last
// checked at or before checkedBefore, never-checked first, then oldest check.
func (r *EpisodeRepository) UpgradeDue(ctx context.Context, checkedBefore time.Time, limit int) ([]model.Episode, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectEpisodeSQL+upgradeDueSQL, FormatTime(checkedBefore), clampUpgradeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("list episodes due for upgrade check: %w", err)
	}
	return collectRows(rows, scanEpisode)
}

// MarkUpgradeChecked records that the movies were examined for an upgrade at.
func (r *MovieRepository) MarkUpgradeChecked(ctx context.Context, ids []int64, at time.Time) error {
	return markUpgradeChecked(ctx, r.s, "movies", ids, at)
}

// MarkUpgradeChecked records that the episodes were examined for an upgrade at.
func (r *EpisodeRepository) MarkUpgradeChecked(ctx context.Context, ids []int64, at time.Time) error {
	return markUpgradeChecked(ctx, r.s, "episodes", ids, at)
}

// ListUpgradeable returns imported movies, and ones being upgraded, whose
// profile sets an upgrade cutoff.
func (r *MovieRepository) ListUpgradeable(ctx context.Context, limit int) ([]model.Movie, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectMovieSQL+`
 WHERE imported_path != '' AND state IN ('imported','wanted','searching','grabbed','downloading','importing')
 AND quality_profile_id IN (SELECT id FROM quality_profiles WHERE upgrade_until IS NOT NULL AND upgrade_until != '')
 ORDER BY sort_title, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list upgradeable movies: %w", err)
	}
	return collectRows(rows, scanMovie)
}

// ListUpgradeable returns imported episodes, and ones being upgraded, whose
// series profile sets an upgrade cutoff.
func (r *EpisodeRepository) ListUpgradeable(ctx context.Context, limit int) ([]model.Episode, error) {
	rows, err := r.s.ro.QueryContext(ctx, selectEpisodeSQL+`
 WHERE imported_path != '' AND state IN ('imported','wanted','searching','grabbed','downloading','importing')
 AND series_id IN (SELECT s.id FROM series s
  JOIN quality_profiles p ON p.id = s.quality_profile_id WHERE p.upgrade_until IS NOT NULL)
 ORDER BY series_id, season, number LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list upgradeable episodes: %w", err)
	}
	return collectRows(rows, scanEpisode)
}

// StartUpgrade returns an imported item to wanted, due immediately, so the
// search loop looks for a better release. The imported file stays in place; a
// search that finds nothing returns the item to imported. It also records the
// upgrade check time. An item that is not imported yields ErrItemBusy, and an
// imported item without a file yields ErrInvalidTransition.
func (r *TransitionRepository) StartUpgrade(ctx context.Context, subject model.SubjectType, id int64, reason, detail string) error {
	lock, err := r.s.Locks().Acquire(ctx, subject, id, "upgrade", time.Minute)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var from model.ItemState
		var importedPath string
		err := tx.QueryRowContext(ctx, `SELECT state, imported_path FROM `+itemTable(subject)+` WHERE id=?`,
			id).Scan(&from, &importedPath)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s:%d: %w", subject, id, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("read %s:%d for upgrade: %w", subject, id, err)
		}
		if from != model.StateImported {
			return fmt.Errorf("%s:%d is %s: %w", subject, id, from, ErrItemBusy)
		}
		if importedPath == "" {
			return fmt.Errorf("%s:%d has no imported file: %w", subject, id, ErrInvalidTransition)
		}
		now := r.s.nowUTC()
		res, err := tx.ExecContext(ctx, `UPDATE `+itemTable(subject)+` SET state='wanted',
 next_search_at=NULL, search_attempts=0, last_error='', upgrade_checked_at=?,
 first_wanted_at=COALESCE(first_wanted_at, ?)
 WHERE id=? AND state='imported'`, FormatTime(now), FormatTime(now), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConflict
		}
		return insertTransition(ctx, tx, subject, id, from, model.StateWanted, reason, detail, now)
	})
}

const upgradeDueSQL = `
 WHERE state='imported' AND imported_path != ''
 AND (upgrade_checked_at IS NULL OR upgrade_checked_at <= ?)
 ORDER BY COALESCE(upgrade_checked_at, ''), id LIMIT ?`

func clampUpgradeLimit(limit int) int {
	if limit <= 0 || limit > upgradeBatchLimit {
		return upgradeBatchLimit
	}
	return limit
}

// inflightUpgrades counts rows of table. table is always a package-internal
// constant, never caller input.
func inflightUpgrades(ctx context.Context, db *sql.DB, table string) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+
		` WHERE imported_path != '' AND state IN `+inflightUpgradeStates).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count in-flight %s upgrades: %w", table, err)
	}
	return count, nil
}

// markUpgradeChecked updates upgrade_checked_at in chunks. table is always a
// package-internal constant, never caller input.
func markUpgradeChecked(ctx context.Context, s *Store, table string, ids []int64, at time.Time) error {
	for _, chunk := range chunkIDs(ids) {
		args := append([]any{FormatTime(at)}, int64Args(chunk)...)
		_, err := s.rw.ExecContext(ctx, `UPDATE `+table+` SET upgrade_checked_at=? WHERE id IN (`+
			placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return fmt.Errorf("mark %s upgrade checked: %w", table, err)
		}
	}
	return nil
}
