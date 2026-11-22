package store

import (
	"context"
	"database/sql"
	"errors"
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

// UserNotification is a leased per-user availability event with the owner's
// destination as it is now. HasDestination is false when the owner deleted it
// after the event was queued.
type UserNotification struct {
	ID             int64
	Payload        string
	Attempts       int
	Destination    NotificationDestination
	HasDestination bool
}

// ClaimUserNotification leases the oldest due per-user event for one minute,
// like ClaimNotification, and joins the owner's current destination.
func (r *RequestRepository) ClaimUserNotification(ctx context.Context) (UserNotification, error) {
	var notification UserNotification
	var err = r.s.InTx(ctx, func(tx *sql.Tx) error {
		var now = r.s.nowUTC()
		var serverID, userID string
		var err = tx.QueryRowContext(ctx, `SELECT o.id,o.payload,o.attempts,mr.server_id,mr.user_id FROM user_availability_outbox o JOIN media_requests mr ON mr.id=o.request_id WHERE o.delivered_at IS NULL AND o.next_attempt_at<=? AND mr.cancelled_at IS NULL ORDER BY o.id LIMIT 1`, FormatTime(now)).Scan(&notification.ID, &notification.Payload, &notification.Attempts, &serverID, &userID)

		if err != nil {
			return err
		}
		var destination, scanErr = scanDestination(tx.QueryRowContext(ctx, `SELECT `+destinationColumns+` FROM notification_destinations WHERE server_id=? AND user_id=? AND enabled=1`, serverID, userID))

		if scanErr == nil {
			notification.Destination, notification.HasDestination = destination, true
		} else if !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		_, err = tx.ExecContext(ctx, `UPDATE user_availability_outbox SET attempts=attempts+1,next_attempt_at=? WHERE id=?`, FormatTime(now.Add(time.Minute)), notification.ID)
		return err
	})

	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return notification, err
}

// FinishUserNotification marks a per-user event delivered when failure is
// empty, and otherwise records the failure and schedules the retry after delay.
func (r *RequestRepository) FinishUserNotification(ctx context.Context, id int64, failure string, delay time.Duration) error {
	var err error

	if failure == "" {
		_, err = r.s.rw.ExecContext(ctx, `UPDATE user_availability_outbox SET delivered_at=?,last_error='' WHERE id=?`, FormatTime(r.s.nowUTC()), id)
	} else {
		_, err = r.s.rw.ExecContext(ctx, `UPDATE user_availability_outbox SET next_attempt_at=?,last_error=? WHERE id=?`, FormatTime(r.s.nowUTC().Add(delay)), failure, id)
	}
	return err
}

// DropUserNotification closes a per-user event that can never be delivered,
// keeping the reason in last_error instead of retrying forever.
func (r *RequestRepository) DropUserNotification(ctx context.Context, id int64, reason string) error {
	var _, err = r.s.rw.ExecContext(ctx, `UPDATE user_availability_outbox SET delivered_at=?,last_error=? WHERE id=?`, FormatTime(r.s.nowUTC()), reason, id)

	return err
}
