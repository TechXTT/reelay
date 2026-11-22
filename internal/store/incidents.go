package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Incident kinds, matching the incidents.kind CHECK constraint.
const (
	IncidentIndexer        = "indexer"
	IncidentDownloadClient = "download_client"
	IncidentStorage        = "storage"
	IncidentLibrary        = "library"
	IncidentMetadata       = "metadata"
	IncidentNotifications  = "notifications"
)

// IncidentKinds lists every kind, so metrics export a zero for a kind
// without open incidents.
var IncidentKinds = []string{IncidentIndexer, IncidentDownloadClient, IncidentStorage,
	IncidentLibrary, IncidentMetadata, IncidentNotifications}

// resolvedIncidentLimit bounds how many resolved incidents are kept when audit
// retention is disabled and nothing else prunes them.
const resolvedIncidentLimit = 200

// Incident is one operator-facing record of a cause that makes many items
// fail or wait. Detail is the raw error and must never reach members.
type Incident struct {
	ID          int64      `json:"id"`
	Kind        string     `json:"kind"`
	Key         string     `json:"cause_key"`
	Detail      string     `json:"detail"`
	Affected    int        `json:"affected"`
	FirstSeenAt time.Time  `json:"first_seen_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	NextCheckAt *time.Time `json:"next_check_at"`
	ResolvedAt  *time.Time `json:"resolved_at"`
}

type IncidentRepository struct{ s *Store }

func (s *Store) Incidents() *IncidentRepository { return &IncidentRepository{s: s} }

const incidentColumns = `id,kind,cause_key,detail,affected,first_seen_at,last_seen_at,next_check_at,resolved_at`

func scanIncident(row scanner) (Incident, error) {
	var value Incident
	var first, last string
	var next, resolved sql.NullString

	err := row.Scan(&value.ID, &value.Kind, &value.Key, &value.Detail, &value.Affected, &first, &last, &next, &resolved)
	if err != nil {
		return value, err
	}
	if value.FirstSeenAt, err = ParseTime(first); err != nil {
		return value, err
	}
	if value.LastSeenAt, err = ParseTime(last); err != nil {
		return value, err
	}
	if value.NextCheckAt, err = scanNullTime(next); err != nil {
		return value, err
	}
	value.ResolvedAt, err = scanNullTime(resolved)
	return value, err
}

// Observe opens the incident for value's cause, or refreshes the open one.
// The affected count never decreases while the incident stays open, and an
// empty detail keeps the recorded one.
func (r *IncidentRepository) Observe(ctx context.Context, value Incident, at time.Time) error {
	_, err := r.s.rw.ExecContext(ctx, `INSERT INTO incidents
 (kind,cause_key,detail,affected,first_seen_at,last_seen_at,next_check_at) VALUES (?,?,?,?,?,?,?)
 ON CONFLICT (kind,cause_key) WHERE resolved_at IS NULL DO UPDATE SET
 detail=CASE WHEN excluded.detail='' THEN incidents.detail ELSE excluded.detail END,
 affected=MAX(incidents.affected,excluded.affected),
 last_seen_at=excluded.last_seen_at,next_check_at=excluded.next_check_at`,
		value.Kind, value.Key, value.Detail, value.Affected, FormatTime(at), FormatTime(at), nullTime(value.NextCheckAt))
	if err != nil {
		return fmt.Errorf("observe %s incident %q: %w", value.Kind, value.Key, err)
	}
	return nil
}

// IncidentCauseRemoved is the note that leads the detail of an incident closed
// because its cause is no longer configured, rather than seen healthy.
const IncidentCauseRemoved = "Cause removed from configuration."

// Resolve closes the open incident of a cause, if any, and trims the oldest
// resolved incidents beyond resolvedIncidentLimit. A non-empty note says why
// it closed and is kept ahead of the recorded detail.
func (r *IncidentRepository) Resolve(ctx context.Context, kind, key, note string, at time.Time) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE incidents SET resolved_at=?, next_check_at=NULL,
 detail=CASE WHEN ?='' THEN detail WHEN detail='' THEN ? ELSE ? || ' Last error: ' || detail END
 WHERE kind=? AND cause_key=? AND resolved_at IS NULL`, FormatTime(at), note, note, note, kind, key)
		if err != nil {
			return fmt.Errorf("resolve %s incident %q: %w", kind, key, err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM incidents WHERE resolved_at IS NOT NULL AND id NOT IN (
 SELECT id FROM incidents WHERE resolved_at IS NOT NULL ORDER BY resolved_at DESC, id DESC LIMIT ?)`, resolvedIncidentLimit)
		if err != nil {
			return fmt.Errorf("trim resolved incidents: %w", err)
		}
		return nil
	})
}

// Open returns every open incident, oldest first.
func (r *IncidentRepository) Open(ctx context.Context) ([]Incident, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT `+incidentColumns+` FROM incidents
 WHERE resolved_at IS NULL ORDER BY first_seen_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list open incidents: %w", err)
	}
	return collectRows(rows, scanIncident)
}

// Resolved returns the most recently resolved incidents, newest first.
func (r *IncidentRepository) Resolved(ctx context.Context, limit int) ([]Incident, error) {
	rows, err := r.s.ro.QueryContext(ctx, `SELECT `+incidentColumns+` FROM incidents
 WHERE resolved_at IS NOT NULL ORDER BY resolved_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list resolved incidents: %w", err)
	}
	return collectRows(rows, scanIncident)
}

// PruneResolvedBefore deletes incidents resolved before cutoff.
func (r *IncidentRepository) PruneResolvedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.s.rw.ExecContext(ctx, `DELETE FROM incidents WHERE resolved_at < ?`, FormatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("prune resolved incidents: %w", err)
	}
	return res.RowsAffected()
}
