package store

import (
	"context"
	"database/sql"
	"time"
)

type AvailabilityNotification struct {
	ID       int64
	Payload  string
	Attempts int
}

func (r *RequestRepository) ClaimNotification(ctx context.Context) (AvailabilityNotification, error) {
	var notification AvailabilityNotification
	var err = r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = r.s.nowUTC()
		var err = tx.QueryRowContext(ctx, `SELECT o.id,o.payload,o.attempts FROM availability_outbox o JOIN media_requests mr ON mr.id=o.request_id WHERE o.delivered_at IS NULL AND o.next_attempt_at<=? AND mr.cancelled_at IS NULL ORDER BY o.id LIMIT 1`, FormatTime(now)).Scan(&notification.ID, &notification.Payload, &notification.Attempts)

		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE availability_outbox SET attempts=attempts+1,next_attempt_at=? WHERE id=?`, FormatTime(now.Add(time.Minute)), notification.ID)
		return err
	})

	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return notification, err
}

func (r *RequestRepository) FinishNotification(ctx context.Context, id int64, failure string, delay time.Duration) error {
	var err error

	if failure == "" {
		_, err = r.s.rw.ExecContext(ctx, `UPDATE availability_outbox SET delivered_at=?,last_error='' WHERE id=?`, FormatTime(r.s.nowUTC()), id)
	} else {
		_, err = r.s.rw.ExecContext(ctx, `UPDATE availability_outbox SET next_attempt_at=?,last_error=? WHERE id=?`, FormatTime(r.s.nowUTC().Add(delay)), failure, id)
	}
	return err
}
