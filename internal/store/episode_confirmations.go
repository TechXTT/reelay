package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EpisodeConfirmationLimit bounds one candidate page and one confirmation batch.
const EpisodeConfirmationLimit = 400

// EpisodeRecheckInterval is how long a confirmation stands before the plugin
// is asked to check that the episode is still visible to the requester.
const EpisodeRecheckInterval = 6 * time.Hour

// EpisodeCandidate is a requested episode Reelay imported that Jellyfin has not
// yet confirmed as visible to the requester.
// Recheck marks an existing confirmation that is due to be checked again.
type EpisodeCandidate struct {
	RequestID int64 `json:"request_id"`
	TMDBID    int   `json:"tmdb_id"`
	Season    int   `json:"season"`
	Number    int   `json:"number"`
	Recheck   bool  `json:"recheck"`
}

// EpisodeKey names a requested episode the plugin no longer finds for the
// requester during a recheck.
type EpisodeKey struct {
	RequestID int64 `json:"request_id"`
	Season    int   `json:"season"`
	Number    int   `json:"number"`
}

// EpisodeConfirmation is the Jellyfin episode item the plugin found for a
// candidate with the requester's access applied.
type EpisodeConfirmation struct {
	RequestID int64  `json:"request_id"`
	Season    int    `json:"season"`
	Number    int    `json:"number"`
	ItemID    string `json:"item_id"`
}

// requestedImportedEpisodes selects (request, episode) pairs in the request's
// scope, the same scope Requests summarizes, whose file Reelay imported.
const requestedImportedEpisodes = `SELECT mr.id AS request_id,mr.tmdb_id,ep.season,ep.number
 FROM media_requests mr JOIN series sr ON mr.subject_type='series' AND sr.id=mr.subject_id AND sr.tmdb_id=mr.tmdb_id
 JOIN episodes ep ON ep.series_id=sr.id
 WHERE mr.server_id=? AND mr.user_id=? AND mr.cancelled_at IS NULL AND ep.state='imported'
   AND (mr.monitor_mode<>'' OR mr.seasons_json='[]' OR ep.season IN (SELECT value FROM json_each(mr.seasons_json)))
   AND (NOT EXISTS(SELECT 1 FROM series_trials t WHERE t.request_id=mr.id AND t.decision<>'continue')
        OR EXISTS(SELECT 1 FROM series_trial_episodes te WHERE te.request_id=mr.id AND te.episode_id=ep.id))`

func episodeConfirmationKey(serverID string) string {
	return "jellyfin.episode_confirmation:" + serverID
}

// SetEpisodeConfirmationSupport records whether the server's plugin confirms
// requested episodes. While it does, a present series parent no longer queues
// a series request's availability event; the first confirmed episode does.
func (r *RecommendationRepository) SetEpisodeConfirmationSupport(ctx context.Context, serverID string, supported bool) error {
	var now = FormatTime(r.s.nowUTC())
	var value = "0"

	if supported {
		value = "1"
	}
	_, err := r.s.rw.ExecContext(ctx, `INSERT INTO kv(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, episodeConfirmationKey(serverID), value, now)
	if err != nil {
		return fmt.Errorf("record episode confirmation support: %w", err)
	}
	return nil
}

// PendingEpisodeConfirmations lists the user's requested, imported episodes
// that Jellyfin has not confirmed yet, then confirmations of the user's active
// requests that are due for a recheck, oldest first within each group.
func (r *RecommendationRepository) PendingEpisodeConfirmations(ctx context.Context, serverID, userID string) ([]EpisodeCandidate, error) {
	var due = FormatTime(r.s.nowUTC().Add(-EpisodeRecheckInterval))
	var rows, err = r.s.ro.QueryContext(ctx, `SELECT request_id,tmdb_id,season,number,recheck FROM (
 SELECT candidate.*,0 AS recheck,'' AS confirmed_at FROM (`+requestedImportedEpisodes+`) candidate
 WHERE NOT EXISTS(SELECT 1 FROM jellyfin_episode_confirmations c WHERE c.request_id=candidate.request_id AND c.season=candidate.season AND c.number=candidate.number)
 UNION ALL
 SELECT c.request_id,mr.tmdb_id,c.season,c.number,1,c.confirmed_at FROM jellyfin_episode_confirmations c JOIN media_requests mr ON mr.id=c.request_id
 WHERE mr.server_id=? AND mr.user_id=? AND mr.cancelled_at IS NULL AND c.confirmed_at<=?
) ORDER BY recheck,confirmed_at,request_id,season,number LIMIT ?`, serverID, userID, serverID, userID, due, EpisodeConfirmationLimit)

	if err != nil {
		return nil, fmt.Errorf("list pending episode confirmations: %w", err)
	}
	return collectRows(rows, func(row scanner) (EpisodeCandidate, error) {
		var value EpisodeCandidate

		return value, row.Scan(&value.RequestID, &value.TMDBID, &value.Season, &value.Number, &value.Recheck)
	})
}

// ConfirmEpisodes stores confirmations that match one of the user's current
// candidates, refreshes ones it already holds, and withdraws the user's
// confirmations for missing episodes. It returns how many confirmations were
// new and how many were withdrawn. Each batch of new confirmations queues one
// availability event per request covering the episodes no earlier event
// announced, so rechecks, repeated syncs and re-confirmed episodes never
// notify again.
func (r *RecommendationRepository) ConfirmEpisodes(ctx context.Context, serverID, userID string, confirmations []EpisodeConfirmation, missing []EpisodeKey) (int, int, error) {
	var confirmed int
	var withdrawn int

	if strings.TrimSpace(serverID) == "" || strings.TrimSpace(userID) == "" {
		return 0, 0, errors.New("confirm episodes requires server id and user id")
	}
	err := r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = FormatTime(r.s.nowUTC())
		var touched = make(map[int64]bool)

		for _, confirmation := range confirmations {
			var result, err = tx.ExecContext(ctx, `UPDATE jellyfin_episode_confirmations SET item_id=?,confirmed_at=?
 WHERE request_id=? AND season=? AND number=? AND request_id IN (SELECT id FROM media_requests WHERE server_id=? AND user_id=?)`,
				confirmation.ItemID, now, confirmation.RequestID, confirmation.Season, confirmation.Number, serverID, userID)

			if err != nil {
				return fmt.Errorf("refresh episode confirmation: %w", err)
			}
			refreshed, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if refreshed > 0 {
				continue
			}
			result, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO jellyfin_episode_confirmations(request_id,season,number,item_id,confirmed_at)
 SELECT candidate.request_id,candidate.season,candidate.number,?,? FROM (`+requestedImportedEpisodes+`) candidate
 WHERE candidate.request_id=? AND candidate.season=? AND candidate.number=?`,
				confirmation.ItemID, now, serverID, userID, confirmation.RequestID, confirmation.Season, confirmation.Number)

			if err != nil {
				return fmt.Errorf("store episode confirmation: %w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count > 0 {
				confirmed++
				touched[confirmation.RequestID] = true
			}
		}
		for _, key := range missing {
			var result, err = tx.ExecContext(ctx, `DELETE FROM jellyfin_episode_confirmations
 WHERE request_id=? AND season=? AND number=? AND request_id IN (SELECT id FROM media_requests WHERE server_id=? AND user_id=?)`,
				key.RequestID, key.Season, key.Number, serverID, userID)

			if err != nil {
				return fmt.Errorf("withdraw episode confirmation: %w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			withdrawn += int(count)
		}
		for requestID := range touched {
			if err := queueEpisodeAvailability(ctx, tx, requestID, now); err != nil {
				return err
			}
		}
		return nil
	})
	return confirmed, withdrawn, err
}

// queueEpisodeAvailability queues one event for the request's confirmed
// episodes that no event has covered yet, naming the lowest of them and how many
// there are, and then records them as covered so no episode is announced twice.
// Unlike title events these need no replay guard: a confirmation is itself the
// moment the episode is first seen, so the requester's current destination gets
// every batch, including the first.
func queueEpisodeAvailability(ctx context.Context, tx *sql.Tx, requestID int64, now string) error {
	const uncovered = `x.request_id=mr.id AND NOT EXISTS(SELECT 1 FROM episode_notifications n WHERE n.request_id=x.request_id AND n.season=x.season AND n.number=x.number)`
	const event = `SELECT mr.id,c.item_id,json_object('event','title_available','request_id',mr.id,'server_id',mr.server_id,'user_id',mr.user_id,'media_type',mr.media_type,'tmdb_id',mr.tmdb_id,'title',mr.title,'jellyfin_item_id',c.item_id,'season',c.season,'episode',c.number,
 'episode_count',(SELECT COUNT(*) FROM jellyfin_episode_confirmations x WHERE ` + uncovered + `)),?,?
 FROM media_requests mr JOIN jellyfin_episode_confirmations c ON c.request_id=mr.id
 AND c.rowid=(SELECT x.rowid FROM jellyfin_episode_confirmations x WHERE ` + uncovered + ` ORDER BY x.season,x.number LIMIT 1)`

	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_availability_outbox(request_id,item_id,payload,created_at,next_attempt_at)
 `+event+`
 JOIN notification_destinations nd ON nd.server_id=mr.server_id AND nd.user_id=mr.user_id AND nd.enabled=1
 WHERE mr.id=? AND mr.cancelled_at IS NULL`, now, now, requestID)
	if err != nil {
		return fmt.Errorf("queue user episode availability: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO availability_outbox(request_id,item_id,payload,created_at,next_attempt_at)
 `+event+`
 WHERE mr.id=? AND mr.cancelled_at IS NULL`, now, now, requestID)
	if err != nil {
		return fmt.Errorf("queue episode availability: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO episode_notifications(request_id,season,number,notified_at)
 SELECT x.request_id,x.season,x.number,? FROM jellyfin_episode_confirmations x JOIN media_requests mr ON mr.id=x.request_id
 WHERE mr.id=? AND mr.cancelled_at IS NULL AND `+uncovered, now, requestID)
	if err != nil {
		return fmt.Errorf("record notified episodes: %w", err)
	}
	return nil
}
