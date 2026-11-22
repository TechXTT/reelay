package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Subtitle coverage of one media source, as the plugin reads it from Jellyfin.
const (
	SubtitlePresent = "present"
	SubtitleMissing = "missing"
	SubtitleUnknown = "unknown"
)

// Outcomes the plugin reports for a leased subtitle job.
const (
	SubtitleDownloaded  = "downloaded"
	SubtitleConfirmed   = "confirmed"
	SubtitleCovered     = "covered"
	SubtitleStale       = "stale"
	SubtitleNoMatch     = "no_match"
	SubtitleWeakMatch   = "weak_match"
	SubtitleNoProvider  = "no_provider"
	SubtitleRateLimited = "rate_limited"
	SubtitleFailed      = "failed"
	SubtitleNotVisible  = "not_visible"
)

const (
	// SubtitleCoverageBatchLimit bounds one coverage batch from the plugin.
	SubtitleCoverageBatchLimit = 500
	// SubtitleBatchLimit bounds one operator-triggered download batch.
	SubtitleBatchLimit = 25
	// SubtitleLeaseLimit bounds how many jobs the plugin takes per pass.
	SubtitleLeaseLimit = 1
	// SubtitleCandidateLimit bounds the weak matches kept for manual selection.
	SubtitleCandidateLimit = 10

	subtitleLease           = 15 * time.Minute
	subtitleVerifyDelay     = 2 * time.Minute
	subtitleVerifyChecks    = 4
	subtitleNoMatchRetry    = 7 * 24 * time.Hour
	subtitleNoMatchAttempts = 4
	subtitleNoProviderRetry = 24 * time.Hour
	subtitleFailureBackoff  = 15 * time.Minute
	subtitleFailureMax      = 24 * time.Hour
	subtitleFailureAttempts = 5
	subtitleDefaultPause    = time.Hour
	subtitleMaxPause        = 24 * time.Hour

	// subtitleLeaseExpired marks a download that was interrupted before the
	// plugin reported it, for example by a restart.
	subtitleLeaseExpired = "lease_expired"
)

// Jobs in these states are still being worked on and are never replaced by a
// new request for the same media source and language.
const subtitleActiveStates = `'queued','running','waiting','needs_review'`

// subtitleReplaceable is the ON CONFLICT condition under which a request
// replaces an existing job: the old job has finished and nothing is scheduled.
const subtitleReplaceable = `subtitle_jobs.state IN ('covered','stale','cancelled','confirmed')
 OR (subtitle_jobs.state IN ('failed','no_match') AND subtitle_jobs.next_attempt_at IS NULL)`

// SubtitleSource is one playable media source in a coverage report. Season
// and Episode are set for episodes only.
type SubtitleSource struct {
	MediaSourceID     string   `json:"media_source_id"`
	ItemID            string   `json:"item_id"`
	MediaType         string   `json:"media_type"`
	Library           string   `json:"library"`
	Title             string   `json:"title"`
	SeriesTitle       string   `json:"series_title"`
	Season            *int     `json:"season"`
	Episode           *int     `json:"episode"`
	Year              int      `json:"year"`
	Edition           string   `json:"edition"`
	Status            string   `json:"status"`
	SubtitleLanguages []string `json:"subtitle_languages"`
	Fingerprint       string   `json:"fingerprint"`
}

// SubtitleLibrary describes a Jellyfin library's own subtitle settings, so the
// operator can see where Jellyfin's scheduled task would also fetch subtitles.
type SubtitleLibrary struct {
	Name                string   `json:"name"`
	DownloadLanguages   []string `json:"download_languages"`
	SaveWithMedia       bool     `json:"save_with_media"`
	RequirePerfectMatch bool     `json:"require_perfect_match"`
}

// SubtitleAudit is what the plugin reports when it completes a coverage pass.
type SubtitleAudit struct {
	Library         string            `json:"audit_library,omitempty"`
	LastFullAuditAt *time.Time        `json:"last_full_audit_at,omitempty"`
	CompletedAt     time.Time         `json:"completed_at"`
	Providers       []string          `json:"providers"`
	Libraries       []SubtitleLibrary `json:"libraries"`
}

// SubtitleJobState is a job's state as shown next to a media source.
type SubtitleJobState struct {
	BlocksBatch bool   `json:"blocks_batch"`
	ID          int64  `json:"id"`
	Language    string `json:"language"`
	State       string `json:"state"`
	Outcome     string `json:"outcome"`
}

// SubtitleCoverage is a stored coverage row with the jobs for its source.
type SubtitleCoverage struct {
	SubtitleSource
	ServerID    string             `json:"server_id"`
	FirstSeenAt time.Time          `json:"first_seen_at"`
	CheckedAt   time.Time          `json:"checked_at"`
	Jobs        []SubtitleJobState `json:"jobs"`
	JellyfinURL string             `json:"jellyfin_url,omitempty"`
}

// SubtitleCandidate is a provider result that was not a confident match.
type SubtitleCandidate struct {
	ID                string `json:"id"`
	Provider          string `json:"provider"`
	Name              string `json:"name"`
	Language          string `json:"language"`
	Format            string `json:"format"`
	HashMatch         bool   `json:"hash_match"`
	Forced            bool   `json:"forced"`
	HearingImpaired   bool   `json:"hearing_impaired"`
	MachineTranslated bool   `json:"machine_translated"`
	Downloads         int    `json:"downloads"`
}

// SubtitleJob is a stored job. Title fields come from the coverage row and are
// empty when the source is no longer in the library.
type SubtitleJob struct {
	ID            int64               `json:"id"`
	ServerID      string              `json:"server_id"`
	MediaSourceID string              `json:"media_source_id"`
	ItemID        string              `json:"item_id"`
	Language      string              `json:"language"`
	Origin        string              `json:"origin"`
	State         string              `json:"state"`
	Outcome       string              `json:"outcome"`
	Detail        string              `json:"detail"`
	Provider      string              `json:"provider"`
	Attempts      int                 `json:"attempts"`
	SubtitleID    string              `json:"subtitle_id"`
	Candidates    []SubtitleCandidate `json:"candidates"`
	NextAttemptAt *time.Time          `json:"next_attempt_at"`
	CreatedAt     time.Time           `json:"created_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
	MediaType     string              `json:"media_type"`
	Title         string              `json:"title"`
	SeriesTitle   string              `json:"series_title"`
	Season        *int                `json:"season"`
	Episode       *int                `json:"episode"`
	Edition       string              `json:"edition"`
}

// SubtitleLeasedJob is a job handed to the plugin. Kind is "download" or
// "verify"; a verify job only checks that a downloaded track is now listed.
type SubtitleLeasedJob struct {
	LeaseToken    string `json:"lease_token"`
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	MediaSourceID string `json:"media_source_id"`
	ItemID        string `json:"item_id"`
	Language      string `json:"language"`
	Fingerprint   string `json:"fingerprint"`
	SubtitleID    string `json:"subtitle_id"`
}

// SubtitleLease is the plugin's work for one pass.
type SubtitleLease struct {
	AuditLibrary   string              `json:"audit_library,omitempty"`
	Jobs           []SubtitleLeasedJob `json:"jobs"`
	AuditRequested bool                `json:"audit_requested"`
	PausedUntil    *time.Time          `json:"paused_until"`
}

// SubtitleResult is the plugin's outcome for one leased job. Coverage, when
// present, is the source's coverage as re-read after the work.
type SubtitleResult struct {
	LeaseToken string              `json:"lease_token"`
	JobID      int64               `json:"job_id"`
	Outcome    string              `json:"outcome"`
	Detail     string              `json:"detail"`
	Provider   string              `json:"provider"`
	Candidates []SubtitleCandidate `json:"candidates"`
	Coverage   *SubtitleSource     `json:"coverage"`
}

// SubtitleCount is one group of a coverage or job summary.
type SubtitleCount struct {
	MediaType string `json:"media_type,omitempty"`
	Status    string `json:"status"`
	Count     int    `json:"count"`
}

// SubtitleOverview summarizes one server's coverage and download work.
type SubtitleOverview struct {
	AuditLibrary   string          `json:"audit_library,omitempty"`
	AuditError     string          `json:"audit_error,omitempty"`
	ServerID       string          `json:"server_id"`
	Servers        []string        `json:"servers"`
	Audit          *SubtitleAudit  `json:"audit"`
	AuditRequested bool            `json:"audit_requested"`
	PausedUntil    *time.Time      `json:"paused_until"`
	Follow         string          `json:"follow_language"`
	Coverage       []SubtitleCount `json:"coverage"`
	Jobs           []SubtitleCount `json:"jobs"`
}

// SubtitleCoverageFilter selects coverage rows for the operator.
type SubtitleCoverageFilter struct {
	ServerID  string
	Library   string
	Status    string
	MediaType string
	Limit     int
	Offset    int
}

type SubtitleRepository struct{ s *Store }

func (s *Store) Subtitles() *SubtitleRepository { return &SubtitleRepository{s: s} }

func subtitleAuditKey(serverID string) string               { return "subtitles.audit:" + serverID }
func subtitleAuditErrorKey(serverID string) string          { return "subtitles.audit_error:" + serverID }
func subtitleRequestKey(serverID string) string             { return "subtitles.audit_requested:" + serverID }
func subtitlePauseKey(serverID string) string               { return "subtitles.paused_until:" + serverID }
func subtitleFollowKey(serverID string) string              { return "subtitles.follow:" + serverID }
func subtitleRetryAt(now time.Time, d time.Duration) string { return FormatTime(now.Add(d)) }

func kvValue(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) (string, bool, error) {
	var value string

	err := q.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", key, err)
	}
	return value, true, nil
}

func setKV(ctx context.Context, tx *sql.Tx, key, value, now string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, now)
	if err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

// enqueueSubtitleJob queues a download unless the same source and language
// already has work in progress. It reports whether a job was queued.
func enqueueSubtitleJob(ctx context.Context, tx *sql.Tx, serverID, mediaSourceID, language, itemID, fingerprint, origin, now string) (bool, error) {
	var result, err = tx.ExecContext(ctx, `INSERT INTO subtitle_jobs(server_id,media_source_id,language,item_id,fingerprint,origin,state,next_attempt_at,created_at,updated_at)
 VALUES(?,?,?,?,?,?,'queued',?,?,?)
 ON CONFLICT(server_id,media_source_id,language) DO UPDATE SET item_id=excluded.item_id,fingerprint=excluded.fingerprint,origin=excluded.origin,
 state='queued',attempts=0,checks=0,subtitle_id='',candidates='[]',outcome='',detail='',provider='',next_attempt_at=excluded.next_attempt_at,lease_until=NULL,updated_at=excluded.updated_at
 WHERE `+subtitleReplaceable, serverID, mediaSourceID, language, itemID, fingerprint, origin, now, now, now)
	var count int64

	if err != nil {
		return false, fmt.Errorf("queue subtitle job: %w", err)
	}
	count, err = result.RowsAffected()
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// StageCoverage stores one batch of a coverage pass. A replaced file marks
// that source's unfinished jobs stale, and present subtitles close jobs that
// have not downloaded anything yet. When new-import following is on and the
// server has completed a pass before, a source that is newly seen, replaced,
// or subsequently probed for one of those imports is queued if missing. It
// returns how many sources were stored and how many jobs were queued.
func (r *SubtitleRepository) StageCoverage(ctx context.Context, serverID, syncToken string, sources []SubtitleSource) (int, int, error) {
	var queued int

	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = FormatTime(r.s.nowUTC())
		var follow, _, err = kvValue(ctx, tx, subtitleFollowKey(serverID))

		if err != nil {
			return err
		}
		rawAudit, audited, err := kvValue(ctx, tx, subtitleAuditKey(serverID))
		if err != nil {
			return err
		}
		if audited {
			var previous SubtitleAudit
			if err := decodeJSON(rawAudit, &previous); err != nil {
				return err
			}
			audited = previous.Library == "" || previous.LastFullAuditAt != nil
		}
		for _, source := range sources {
			var previousFingerprint string
			var followPending bool
			var languages string
			var seen = true

			err = tx.QueryRowContext(ctx, `SELECT fingerprint,follow_pending FROM subtitle_coverage WHERE server_id=? AND media_source_id=?`,
				serverID, source.MediaSourceID).Scan(&previousFingerprint, &followPending)
			if errors.Is(err, sql.ErrNoRows) {
				seen = false
			} else if err != nil {
				return fmt.Errorf("read subtitle coverage: %w", err)
			}
			replaced := seen && previousFingerprint != source.Fingerprint
			if !seen || replaced {
				followPending = audited && follow != ""
			}
			queueImport := followPending && follow != "" && source.Status == SubtitleMissing
			followPending = followPending && follow != "" && source.Status == SubtitleUnknown
			languages, err = encodeJSON(nonNilStrings(source.SubtitleLanguages))
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO subtitle_coverage(server_id,media_source_id,item_id,media_type,library,title,series_title,season,episode,year,edition,status,subtitle_languages,fingerprint,sync_token,first_seen_at,checked_at,follow_pending)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(server_id,media_source_id) DO UPDATE SET item_id=excluded.item_id,media_type=excluded.media_type,library=excluded.library,title=excluded.title,
 series_title=excluded.series_title,season=excluded.season,episode=excluded.episode,year=excluded.year,edition=excluded.edition,status=excluded.status,
 subtitle_languages=excluded.subtitle_languages,fingerprint=excluded.fingerprint,sync_token=excluded.sync_token,checked_at=excluded.checked_at,follow_pending=excluded.follow_pending`,
				serverID, source.MediaSourceID, source.ItemID, source.MediaType, source.Library, source.Title, source.SeriesTitle, source.Season, source.Episode,
				source.Year, source.Edition, source.Status, languages, source.Fingerprint, syncToken, now, now, followPending)
			if err != nil {
				return fmt.Errorf("store subtitle coverage: %w", err)
			}
			if replaced {
				_, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state='stale',outcome='file_replaced',next_attempt_at=NULL,lease_until=NULL,updated_at=?
 WHERE server_id=? AND media_source_id=? AND fingerprint<>? AND (state IN (`+subtitleActiveStates+`) OR (state IN ('failed','no_match') AND next_attempt_at IS NOT NULL))`,
					now, serverID, source.MediaSourceID, source.Fingerprint)
				if err != nil {
					return fmt.Errorf("invalidate subtitle jobs: %w", err)
				}
			}
			if source.Status == SubtitlePresent {
				_, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state='covered',outcome='subtitles_present',next_attempt_at=NULL,lease_until=NULL,updated_at=?
 WHERE server_id=? AND media_source_id=? AND (state IN ('queued','needs_review') OR (state IN ('failed','no_match') AND next_attempt_at IS NOT NULL))`,
					now, serverID, source.MediaSourceID)
				if err != nil {
					return fmt.Errorf("close covered subtitle jobs: %w", err)
				}
			}
			if !queueImport {
				continue
			}
			added, err := enqueueSubtitleJob(ctx, tx, serverID, source.MediaSourceID, follow, source.ItemID, source.Fingerprint, "import", now)
			if err != nil {
				return err
			}
			if added {
				queued++
			}
		}
		return nil
	})
	return len(sources), queued, err
}

// CompleteCoverage finishes a coverage pass: sources the pass did not see are
// removed and their unfinished jobs become stale, the audit summary is stored,
// and an operator's audit request is cleared. It returns how many sources
// were removed.
func (r *SubtitleRepository) CompleteCoverage(ctx context.Context, serverID, syncToken string, audit SubtitleAudit, auditError string) (int, error) {
	var removed int64

	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = FormatTime(r.s.nowUTC())
		var encoded string
		var result sql.Result
		var err error

		requestValue := "1"
		if audit.Library != "" {
			requestValue = "library:" + audit.Library
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE key=? AND value=?`, subtitleRequestKey(serverID), requestValue); err != nil {
			return err
		}
		if auditError != "" {
			return setKV(ctx, tx, subtitleAuditErrorKey(serverID), auditError, now)
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, subtitleAuditErrorKey(serverID)); err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state='stale',outcome='source_removed',next_attempt_at=NULL,lease_until=NULL,updated_at=?
 WHERE server_id=? AND (state IN (`+subtitleActiveStates+`) OR (state IN ('failed','no_match') AND next_attempt_at IS NOT NULL))
 AND media_source_id IN (SELECT media_source_id FROM subtitle_coverage WHERE server_id=? AND sync_token<>? AND (?='' OR library=?))`, now, serverID, serverID, syncToken, audit.Library, audit.Library)
		if err != nil {
			return fmt.Errorf("invalidate removed subtitle jobs: %w", err)
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM subtitle_coverage WHERE server_id=? AND sync_token<>? AND (?='' OR library=?)`, serverID, syncToken, audit.Library, audit.Library)
		if err != nil {
			return fmt.Errorf("remove unseen subtitle coverage: %w", err)
		}
		removed, err = result.RowsAffected()
		if err != nil {
			return err
		}
		audit.CompletedAt = r.s.nowUTC()
		if audit.Library == "" {
			audit.LastFullAuditAt = &audit.CompletedAt
		} else {
			raw, found, err := kvValue(ctx, tx, subtitleAuditKey(serverID))
			if err != nil {
				return err
			}
			if found {
				var previous SubtitleAudit
				if err := decodeJSON(raw, &previous); err != nil {
					return err
				}
				audit.LastFullAuditAt = previous.LastFullAuditAt
				if previous.Library == "" {
					audit.LastFullAuditAt = &previous.CompletedAt
				}
			}
		}
		audit.normalizeLists()
		encoded, err = encodeJSON(audit)
		if err != nil {
			return err
		}
		return setKV(ctx, tx, subtitleAuditKey(serverID), encoded, now)
	})
	return int(removed), err
}

func (audit *SubtitleAudit) normalizeLists() {
	audit.Providers = nonNilStrings(audit.Providers)
	if audit.Libraries == nil {
		audit.Libraries = []SubtitleLibrary{}
	}
	for i := range audit.Libraries {
		audit.Libraries[i].DownloadLanguages = nonNilStrings(audit.Libraries[i].DownloadLanguages)
	}
}

// InvalidateCoverage handles a completed sync from a plugin without subtitle
// reporting. Keep source identities and follow settings for the next baseline.
func (r *SubtitleRepository) InvalidateCoverage(ctx context.Context, serverID string) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		now := FormatTime(r.s.nowUTC())
		if _, err := tx.ExecContext(ctx, `UPDATE subtitle_coverage SET status='unknown',subtitle_languages='[]',follow_pending=0 WHERE server_id=? AND (status<>'unknown' OR follow_pending=1 OR subtitle_languages<>'[]')`, serverID); err != nil {
			return fmt.Errorf("invalidate subtitle coverage after plugin downgrade: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state='stale',outcome='unsupported_plugin',detail='Plugin no longer reports subtitle coverage',next_attempt_at=NULL,lease_until=NULL,updated_at=?
 WHERE server_id=? AND (state IN (`+subtitleActiveStates+`) OR (state IN ('failed','no_match') AND next_attempt_at IS NOT NULL))`, now, serverID); err != nil {
			return fmt.Errorf("invalidate subtitle jobs after plugin downgrade: %w", err)
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, subtitleAuditKey(serverID))
		return err
	})
}

// RequestAudit asks the server's plugin to run a coverage pass on its next check.
func (r *SubtitleRepository) RequestAudit(ctx context.Context, serverID, library string) error {
	value := "1"
	if library != "" {
		value = "library:" + library
	}
	return r.s.KV().Set(ctx, subtitleRequestKey(serverID), value)
}

// SetFollow sets the language queued for new media without subtitles; an
// empty language turns following off.
func (r *SubtitleRepository) SetFollow(ctx context.Context, serverID, language string) error {
	if language == "" {
		return r.s.KV().Delete(ctx, subtitleFollowKey(serverID))
	}
	return r.s.KV().Set(ctx, subtitleFollowKey(serverID), language)
}

// QueueBatch queues downloads in language for sources currently missing
// subtitles: the listed sources, or when none are listed the first missing
// sources in library order. Sources with work in progress are skipped. It
// returns how many jobs were queued.
func (r *SubtitleRepository) QueueBatch(ctx context.Context, filter SubtitleCoverageFilter, language string, mediaSourceIDs []string) (int, error) {
	var queued int

	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = FormatTime(r.s.nowUTC())
		var query = `SELECT c.media_source_id,c.item_id,c.fingerprint FROM subtitle_coverage c
 WHERE c.server_id=? AND c.status='missing' AND (?='' OR c.library=?) AND (?='' OR c.media_type=?)
 AND NOT EXISTS(SELECT 1 FROM subtitle_jobs j WHERE j.server_id=c.server_id AND j.media_source_id=c.media_source_id AND j.language=?
   AND NOT (` + strings.ReplaceAll(subtitleReplaceable, "subtitle_jobs.", "j.") + `))`
		var args = []any{filter.ServerID, filter.Library, filter.Library, filter.MediaType, filter.MediaType, language}
		var rows *sql.Rows
		var err error

		type candidate struct{ mediaSourceID, itemID, fingerprint string }
		if len(mediaSourceIDs) > 0 {
			query += ` AND c.media_source_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(mediaSourceIDs)), ",") + `)`
			for _, id := range mediaSourceIDs {
				args = append(args, id)
			}
		}
		if len(mediaSourceIDs) == 0 {
			// An automatic selection must advance past previous attempts. The operator
			// can explicitly retry an exhausted job after correcting its cause.
			query += ` AND NOT EXISTS (SELECT 1 FROM subtitle_jobs j WHERE j.server_id=c.server_id AND j.media_source_id=c.media_source_id AND j.language=? AND j.fingerprint=c.fingerprint AND j.state IN ('failed','no_match','cancelled'))`
			args = append(args, language)
		}
		query += ` ORDER BY c.media_type,c.series_title,c.season,c.episode,c.title,c.edition,c.media_source_id LIMIT ?`
		args = append(args, filter.Limit)
		rows, err = tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("select subtitle batch: %w", err)
		}
		candidates, err := collectRows(rows, func(row scanner) (candidate, error) {
			var value candidate

			return value, row.Scan(&value.mediaSourceID, &value.itemID, &value.fingerprint)
		})
		if err != nil {
			return err
		}
		for _, value := range candidates {
			added, err := enqueueSubtitleJob(ctx, tx, filter.ServerID, value.mediaSourceID, language, value.itemID, value.fingerprint, "backfill", now)
			if err != nil {
				return err
			}
			if added {
				queued++
			}
		}
		return nil
	})
	return queued, err
}

// Lease hands the plugin up to limit due jobs and marks them running (or keeps
// them waiting for a verify) until the lease expires. An expired lease makes
// the job due again, so work resumes after a restart of either side. Nothing
// is leased while the server is paused after a provider limit.
func (r *SubtitleRepository) Lease(ctx context.Context, serverID string, limit int) (SubtitleLease, error) {
	var lease = SubtitleLease{Jobs: []SubtitleLeasedJob{}}

	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = r.s.nowUTC()
		var paused, found, err = kvValue(ctx, tx, subtitlePauseKey(serverID))
		var rows *sql.Rows

		if err != nil {
			return err
		}
		rawRequest, requested, err := kvValue(ctx, tx, subtitleRequestKey(serverID))
		if err != nil {
			return err
		}
		lease.AuditRequested = requested
		if strings.HasPrefix(rawRequest, "library:") {
			lease.AuditLibrary = strings.TrimPrefix(rawRequest, "library:")
		}
		if found && paused > FormatTime(now) {
			until, err := ParseTime(paused)
			if err != nil {
				return err
			}
			lease.PausedUntil = &until
			return nil
		}
		rows, err = tx.QueryContext(ctx, `SELECT id,state,media_source_id,item_id,language,fingerprint,subtitle_id FROM subtitle_jobs j
 WHERE server_id=? AND next_attempt_at IS NOT NULL AND next_attempt_at<=? AND state IN ('queued','running','waiting','failed','no_match')
 AND NOT EXISTS (SELECT 1 FROM subtitle_jobs other WHERE other.server_id=j.server_id AND other.media_source_id=j.media_source_id
 AND other.id<>j.id AND other.state IN ('running','waiting') AND (j.state NOT IN ('running','waiting') OR other.id<j.id))
 ORDER BY next_attempt_at,id LIMIT ?`, serverID, FormatTime(now), min(limit, SubtitleLeaseLimit))
		if err != nil {
			return fmt.Errorf("select due subtitle jobs: %w", err)
		}
		jobs, err := collectRows(rows, func(row scanner) (SubtitleLeasedJob, error) {
			var value SubtitleLeasedJob
			var state string

			err := row.Scan(&value.ID, &state, &value.MediaSourceID, &value.ItemID, &value.Language, &value.Fingerprint, &value.SubtitleID)
			value.Kind = "download"
			if state == "waiting" || state == "running" {
				value.Kind = "verify"
			}
			return value, err
		})
		if err != nil {
			return err
		}
		until := subtitleRetryAt(now, subtitleLease)
		for i := range jobs {
			job := &jobs[i]
			// A download whose lease expired may or may not have saved a file. It is
			// verified first so nothing is fetched twice, and marked so that a verify
			// that never finds the track queues the download again instead of failing.
			err = tx.QueryRowContext(ctx, `UPDATE subtitle_jobs SET state=CASE WHEN ?='verify' THEN 'waiting' ELSE 'running' END,
 outcome=CASE WHEN state='running' THEN ? ELSE outcome END,checks=CASE WHEN state='running' THEN 0 ELSE checks END,
 lease_token=lower(hex(randomblob(16))),lease_until=?,next_attempt_at=?,updated_at=? WHERE id=? RETURNING lease_token`,
				job.Kind, subtitleLeaseExpired, until, until, FormatTime(now), job.ID).Scan(&job.LeaseToken)
			if err != nil {
				return fmt.Errorf("lease subtitle job: %w", err)
			}
		}
		lease.Jobs = append(lease.Jobs, jobs...)
		return nil
	})
	return lease, err
}

// Report applies the plugin's outcomes to leased jobs. Results for jobs that
// are no longer leased (cancelled meanwhile, or from another server) are
// ignored. A provider limit pauses the whole server for pause, or an hour when
// pause is zero. It returns how many results were applied.
func (r *SubtitleRepository) Report(ctx context.Context, serverID string, results []SubtitleResult, pause time.Duration) (int, error) {
	var applied int

	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = r.s.nowUTC()
		var stamp = FormatTime(now)
		var limited bool

		for _, result := range results {
			var state, sourceID, fingerprint, previousOutcome string
			var attempts int
			var checks int
			var next any
			var candidates = "[]"
			var outcome = result.Outcome
			var err = tx.QueryRowContext(ctx, `SELECT state,attempts,checks,media_source_id,fingerprint,outcome FROM subtitle_jobs WHERE id=? AND server_id=? AND lease_token=? AND lease_token<>'' AND lease_until>? AND state IN ('running','waiting')`,
				result.JobID, serverID, result.LeaseToken, stamp).Scan(&state, &attempts, &checks, &sourceID, &fingerprint, &previousOutcome)

			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read subtitle job: %w", err)
			}
			if result.Coverage != nil && (result.Coverage.MediaSourceID != sourceID || (result.Coverage.Fingerprint != fingerprint && outcome != SubtitleStale)) {
				continue
			}
			if outcome == SubtitleConfirmed && (result.Coverage == nil || result.Coverage.Status != SubtitlePresent || result.Coverage.Fingerprint != fingerprint) {
				continue
			}
			if state == "waiting" {
				switch outcome {
				case SubtitleConfirmed, SubtitleStale, SubtitleNotVisible:
				case SubtitleFailed:
					outcome = SubtitleNotVisible
				default:
					continue
				}
			} else if outcome == SubtitleConfirmed || outcome == SubtitleNotVisible {
				continue
			}
			switch outcome {
			case SubtitleDownloaded:
				state, attempts, checks, next = "waiting", attempts+1, 0, subtitleRetryAt(now, subtitleVerifyDelay)
			case SubtitleConfirmed, SubtitleCovered, SubtitleStale:
				state, next = result.Outcome, nil
			case SubtitleNoMatch:
				state, attempts = "no_match", attempts+1
				if attempts < subtitleNoMatchAttempts {
					next = subtitleRetryAt(now, subtitleNoMatchRetry)
				}
			case SubtitleWeakMatch:
				state, attempts, next = "needs_review", attempts+1, nil
				if len(result.Candidates) > SubtitleCandidateLimit {
					result.Candidates = result.Candidates[:SubtitleCandidateLimit]
				}
				candidates, err = encodeJSON(result.Candidates)
				if err != nil {
					return err
				}
			case SubtitleRateLimited:
				limited = true
				state, next = "queued", nil
			case SubtitleNoProvider, SubtitleFailed:
				state, attempts = "failed", attempts+1
				if attempts < subtitleFailureAttempts {
					next = subtitleRetryAt(now, subtitleFailureDelay(result.Outcome, attempts))
				}
			case SubtitleNotVisible:
				checks++
				state = "waiting"
				next = subtitleRetryAt(now, subtitleVerifyDelay<<checks)
				switch {
				case previousOutcome == subtitleLeaseExpired && checks >= subtitleVerifyChecks:
					// The interrupted download evidently never saved a file; download again.
					state, checks, outcome, next = "queued", 0, subtitleLeaseExpired, stamp
				case previousOutcome == subtitleLeaseExpired:
					outcome = subtitleLeaseExpired
				case checks >= subtitleVerifyChecks:
					state, outcome, next = "failed", "track_not_listed", nil
				}
			default:
				return fmt.Errorf("unknown subtitle outcome %q", result.Outcome)
			}
			if result.Outcome == SubtitleRateLimited {
				// Due again once the pause below ends; the attempt does not count.
				next = stamp
			}
			_, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state=?,attempts=?,checks=?,outcome=?,detail=?,
 provider=CASE WHEN ?<>'' THEN ? ELSE provider END,candidates=CASE WHEN ?='needs_review' THEN ? ELSE candidates END,
 next_attempt_at=?,lease_until=NULL,updated_at=? WHERE id=?`,
				state, attempts, checks, outcome, truncate(result.Detail, 2000), result.Provider, result.Provider, state, candidates, next, stamp, result.JobID)
			if err != nil {
				return fmt.Errorf("record subtitle outcome: %w", err)
			}
			switch {
			case outcome == SubtitleStale && (result.Coverage == nil || result.Coverage.Fingerprint != fingerprint):
				// Reconcile the disappeared or replaced source across every language on the next pass.
				if _, err = tx.ExecContext(ctx, `UPDATE subtitle_coverage SET status='unknown',subtitle_languages='[]' WHERE server_id=? AND media_source_id=? AND fingerprint=?`, serverID, sourceID, fingerprint); err != nil {
					return err
				}
				if err = setKV(ctx, tx, subtitleRequestKey(serverID), "1", stamp); err != nil {
					return err
				}
			case result.Coverage != nil:
				if err = refreshSubtitleCoverage(ctx, tx, serverID, *result.Coverage, stamp); err != nil {
					return err
				}
			}
			applied++
		}
		if !limited {
			return nil
		}
		if pause <= 0 {
			pause = subtitleDefaultPause
		}
		until := FormatTime(now.Add(min(pause, subtitleMaxPause)))
		previous, _, err := kvValue(ctx, tx, subtitlePauseKey(serverID))
		if err != nil {
			return err
		}
		until = max(until, previous)
		_, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET next_attempt_at=? WHERE server_id=? AND state='queued' AND next_attempt_at<?`, until, serverID, until)
		if err != nil {
			return fmt.Errorf("defer subtitle jobs: %w", err)
		}
		return setKV(ctx, tx, subtitlePauseKey(serverID), until, stamp)
	})
	return applied, err
}

func subtitleFailureDelay(outcome string, attempts int) time.Duration {
	if outcome == SubtitleNoProvider {
		return subtitleNoProviderRetry
	}
	return min(subtitleFailureBackoff<<(attempts-1), subtitleFailureMax)
}

// refreshSubtitleCoverage updates a known source's coverage from a job's re-read.
func refreshSubtitleCoverage(ctx context.Context, tx *sql.Tx, serverID string, source SubtitleSource, now string) error {
	var languages, err = encodeJSON(nonNilStrings(source.SubtitleLanguages))

	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE subtitle_coverage SET status=?,subtitle_languages=?,checked_at=? WHERE server_id=? AND media_source_id=? AND fingerprint=?`,
		source.Status, languages, now, serverID, source.MediaSourceID, source.Fingerprint)
	if err != nil {
		return fmt.Errorf("refresh subtitle coverage: %w", err)
	}
	return nil
}

// JobAction applies an operator action to a job. "retry" queues it again
// against the source's current file, which must still lack subtitles;
// "choose" downloads one of a reviewed job's candidates; "cancel" stops
// unfinished work. It returns ErrNotFound or ErrConflict.
func (r *SubtitleRepository) JobAction(ctx context.Context, id int64, action, subtitleID string) error {
	return r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = FormatTime(r.s.nowUTC())
		var state string
		var raw string
		var candidates []SubtitleCandidate
		var result sql.Result
		var count int64
		var err = tx.QueryRowContext(ctx, `SELECT state,candidates FROM subtitle_jobs WHERE id=?`, id).Scan(&state, &raw)

		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read subtitle job: %w", err)
		}
		switch action {
		case "retry":
			result, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state='queued',attempts=0,checks=0,subtitle_id='',candidates='[]',outcome='',detail='',
 next_attempt_at=?,lease_until=NULL,updated_at=?,
 fingerprint=(SELECT c.fingerprint FROM subtitle_coverage c WHERE c.server_id=subtitle_jobs.server_id AND c.media_source_id=subtitle_jobs.media_source_id),
 item_id=(SELECT c.item_id FROM subtitle_coverage c WHERE c.server_id=subtitle_jobs.server_id AND c.media_source_id=subtitle_jobs.media_source_id)
 WHERE id=? AND state IN ('failed','no_match','needs_review','stale','cancelled','covered')
 AND EXISTS(SELECT 1 FROM subtitle_coverage c WHERE c.server_id=subtitle_jobs.server_id AND c.media_source_id=subtitle_jobs.media_source_id AND c.status='missing')`,
				now, now, id)
		case "choose":
			if err = decodeJSON(raw, &candidates); err != nil {
				return err
			}
			if state != "needs_review" || !containsCandidate(candidates, subtitleID) {
				return ErrConflict
			}
			result, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state='queued',subtitle_id=?,outcome='',detail='',next_attempt_at=?,lease_until=NULL,updated_at=? WHERE id=?`,
				subtitleID, now, now, id)
		case "cancel":
			result, err = tx.ExecContext(ctx, `UPDATE subtitle_jobs SET state='cancelled',outcome='cancelled',next_attempt_at=NULL,lease_until=NULL,updated_at=?
 WHERE id=? AND state IN ('queued','running','waiting','needs_review','failed','no_match')`, now, id)
		default:
			return fmt.Errorf("unknown subtitle job action %q", action)
		}
		if err != nil {
			return fmt.Errorf("update subtitle job: %w", err)
		}
		count, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			return ErrConflict
		}
		return nil
	})
}

func containsCandidate(candidates []SubtitleCandidate, id string) bool {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

// Overview summarizes the server's coverage and jobs. An empty serverID picks
// the first server that reported coverage, else the first synchronized one.
func (r *SubtitleRepository) Overview(ctx context.Context, serverID string) (SubtitleOverview, error) {
	var overview = SubtitleOverview{Servers: []string{}, Coverage: []SubtitleCount{}, Jobs: []SubtitleCount{}}
	var rows *sql.Rows
	var raw string
	var found bool
	var err error

	rows, err = r.s.ro.QueryContext(ctx, `SELECT server_id FROM (SELECT DISTINCT server_id,0 AS priority FROM subtitle_coverage
 UNION SELECT DISTINCT server_id,1 FROM jellyfin_users) GROUP BY server_id ORDER BY MIN(priority),server_id`)
	if err != nil {
		return overview, fmt.Errorf("list subtitle servers: %w", err)
	}
	overview.Servers, err = collectRows(rows, scanColumn[string])
	if err != nil {
		return overview, err
	}
	if overview.Servers == nil {
		overview.Servers = []string{}
	}
	if serverID == "" && len(overview.Servers) > 0 {
		serverID = overview.Servers[0]
	}
	overview.ServerID = serverID
	if serverID == "" {
		return overview, nil
	}
	raw, found, err = kvValue(ctx, r.s.ro, subtitleAuditKey(serverID))
	if err != nil {
		return overview, err
	}
	if found {
		overview.Audit = &SubtitleAudit{}
		if err = decodeJSON(raw, overview.Audit); err != nil {
			return overview, err
		}
		overview.Audit.normalizeLists()
	}
	raw, overview.AuditRequested, err = kvValue(ctx, r.s.ro, subtitleRequestKey(serverID))
	if err != nil {
		return overview, err
	}
	if strings.HasPrefix(raw, "library:") {
		overview.AuditLibrary = strings.TrimPrefix(raw, "library:")
	}
	overview.AuditError, _, err = kvValue(ctx, r.s.ro, subtitleAuditErrorKey(serverID))
	if err != nil {
		return overview, err
	}
	raw, found, err = kvValue(ctx, r.s.ro, subtitlePauseKey(serverID))
	if err != nil {
		return overview, err
	}
	if found && raw > FormatTime(r.s.nowUTC()) {
		until, err := ParseTime(raw)
		if err != nil {
			return overview, err
		}
		overview.PausedUntil = &until
	}
	overview.Follow, _, err = kvValue(ctx, r.s.ro, subtitleFollowKey(serverID))
	if err != nil {
		return overview, err
	}
	rows, err = r.s.ro.QueryContext(ctx, `SELECT media_type,status,COUNT(*) FROM subtitle_coverage WHERE server_id=? GROUP BY media_type,status ORDER BY media_type,status`, serverID)
	if err != nil {
		return overview, fmt.Errorf("count subtitle coverage: %w", err)
	}
	overview.Coverage, err = collectRows(rows, func(row scanner) (SubtitleCount, error) {
		var value SubtitleCount

		return value, row.Scan(&value.MediaType, &value.Status, &value.Count)
	})
	if err != nil {
		return overview, err
	}
	rows, err = r.s.ro.QueryContext(ctx, `SELECT state,COUNT(*) FROM subtitle_jobs WHERE server_id=? GROUP BY state ORDER BY state`, serverID)
	if err != nil {
		return overview, fmt.Errorf("count subtitle jobs: %w", err)
	}
	overview.Jobs, err = collectRows(rows, func(row scanner) (SubtitleCount, error) {
		var value SubtitleCount

		return value, row.Scan(&value.Status, &value.Count)
	})
	if overview.Coverage == nil {
		overview.Coverage = []SubtitleCount{}
	}
	if overview.Jobs == nil {
		overview.Jobs = []SubtitleCount{}
	}
	return overview, err
}

// ListCoverage pages through a server's coverage in library order and returns
// the total matching the filter. Each row carries its jobs.
func (r *SubtitleRepository) ListCoverage(ctx context.Context, filter SubtitleCoverageFilter) ([]SubtitleCoverage, int, error) {
	var where = `WHERE server_id=? AND (?='' OR status=?) AND (?='' OR media_type=?) AND (?='' OR library=?)`
	var args = []any{filter.ServerID, filter.Status, filter.Status, filter.MediaType, filter.MediaType, filter.Library, filter.Library}
	var total int
	var rows *sql.Rows
	var values []SubtitleCoverage
	var index = make(map[string]int)
	var err = r.s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM subtitle_coverage `+where, args...).Scan(&total)

	if err != nil {
		return nil, 0, fmt.Errorf("count subtitle coverage: %w", err)
	}
	rows, err = r.s.ro.QueryContext(ctx, `SELECT server_id,media_source_id,item_id,media_type,library,title,series_title,season,episode,year,edition,status,subtitle_languages,fingerprint,first_seen_at,checked_at
 FROM subtitle_coverage `+where+` ORDER BY media_type,series_title,season,episode,title,edition,media_source_id LIMIT ? OFFSET ?`,
		append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("list subtitle coverage: %w", err)
	}
	values, err = collectRows(rows, func(row scanner) (SubtitleCoverage, error) {
		var value SubtitleCoverage
		var season sql.NullInt64
		var episode sql.NullInt64
		var languages string
		var firstSeen string
		var checked string

		err := row.Scan(&value.ServerID, &value.MediaSourceID, &value.ItemID, &value.MediaType, &value.Library, &value.Title, &value.SeriesTitle,
			&season, &episode, &value.Year, &value.Edition, &value.Status, &languages, &value.Fingerprint, &firstSeen, &checked)
		if err != nil {
			return value, err
		}
		value.Season = nullInt(season)
		value.Episode = nullInt(episode)
		value.Jobs = []SubtitleJobState{}
		if err = decodeJSON(languages, &value.SubtitleLanguages); err != nil {
			return value, err
		}
		if value.FirstSeenAt, err = ParseTime(firstSeen); err != nil {
			return value, err
		}
		value.CheckedAt, err = ParseTime(checked)
		return value, err
	})
	if err != nil || len(values) == 0 {
		return []SubtitleCoverage{}, total, err
	}
	ids := make([]any, 0, len(values)+1)
	ids = append(ids, filter.ServerID)
	for i, value := range values {
		index[value.MediaSourceID] = i
		ids = append(ids, value.MediaSourceID)
	}
	rows, err = r.s.ro.QueryContext(ctx, `SELECT media_source_id,id,language,state,outcome,NOT (`+subtitleReplaceable+`) FROM subtitle_jobs WHERE server_id=? AND media_source_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")+`) ORDER BY language`, ids...)
	if err != nil {
		return nil, 0, fmt.Errorf("list subtitle coverage jobs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mediaSourceID string
		var job SubtitleJobState

		if err = rows.Scan(&mediaSourceID, &job.ID, &job.Language, &job.State, &job.Outcome, &job.BlocksBatch); err != nil {
			return nil, 0, err
		}
		values[index[mediaSourceID]].Jobs = append(values[index[mediaSourceID]].Jobs, job)
	}
	return values, total, rows.Err()
}

// ListJobs pages through a server's jobs, most recently updated first,
// optionally limited to one state, and returns the total.
func (r *SubtitleRepository) ListJobs(ctx context.Context, serverID, state string, limit, offset int) ([]SubtitleJob, int, error) {
	var total int
	var rows *sql.Rows
	var values []SubtitleJob
	var err = r.s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM subtitle_jobs WHERE server_id=? AND (?='' OR state=?)`, serverID, state, state).Scan(&total)

	if err != nil {
		return nil, 0, fmt.Errorf("count subtitle jobs: %w", err)
	}
	rows, err = r.s.ro.QueryContext(ctx, `SELECT j.id,j.server_id,j.media_source_id,j.item_id,j.language,j.origin,j.state,j.outcome,j.detail,j.provider,j.attempts,j.subtitle_id,
 j.candidates,j.next_attempt_at,j.created_at,j.updated_at,COALESCE(c.media_type,''),COALESCE(c.title,''),COALESCE(c.series_title,''),c.season,c.episode,COALESCE(c.edition,'')
 FROM subtitle_jobs j LEFT JOIN subtitle_coverage c ON c.server_id=j.server_id AND c.media_source_id=j.media_source_id
 WHERE j.server_id=? AND (?='' OR j.state=?) ORDER BY j.updated_at DESC,j.id DESC LIMIT ? OFFSET ?`, serverID, state, state, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list subtitle jobs: %w", err)
	}
	values, err = collectRows(rows, func(row scanner) (SubtitleJob, error) {
		var value SubtitleJob
		var candidates string
		var next sql.NullString
		var created string
		var updated string
		var season sql.NullInt64
		var episode sql.NullInt64

		err := row.Scan(&value.ID, &value.ServerID, &value.MediaSourceID, &value.ItemID, &value.Language, &value.Origin, &value.State, &value.Outcome,
			&value.Detail, &value.Provider, &value.Attempts, &value.SubtitleID, &candidates, &next, &created, &updated,
			&value.MediaType, &value.Title, &value.SeriesTitle, &season, &episode, &value.Edition)
		if err != nil {
			return value, err
		}
		value.Season = nullInt(season)
		value.Episode = nullInt(episode)
		if err = decodeJSON(candidates, &value.Candidates); err != nil {
			return value, err
		}
		if value.NextAttemptAt, err = scanNullTime(next); err != nil {
			return value, err
		}
		if value.CreatedAt, err = ParseTime(created); err != nil {
			return value, err
		}
		value.UpdatedAt, err = ParseTime(updated)
		return value, err
	})
	if values == nil {
		values = []SubtitleJob{}
	}
	return values, total, err
}

func nullInt(value sql.NullInt64) *int {
	var number int

	if !value.Valid {
		return nil
	}
	number = int(value.Int64)
	return &number
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
