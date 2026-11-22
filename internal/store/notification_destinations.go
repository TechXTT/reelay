package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// NotificationDestination is where one Jellyfin user's availability messages go.
// Kind "ntfy" publishes to a topic on the configured per-user ntfy server;
// kind "webhook" posts to URL and is set by operators only.
type NotificationDestination struct {
	ServerID  string    `json:"server_id"`
	UserID    string    `json:"user_id"`
	Kind      string    `json:"kind"`
	Topic     string    `json:"topic"`
	URL       string    `json:"url"`
	Format    string    `json:"format"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UserDelivery is the latest per-user outbox row for a user's requests.
type UserDelivery struct {
	DeliveredAt *time.Time
	LastError   string
	Attempts    int
}

type NotificationDestinationRepository struct{ s *Store }

func (s *Store) NotificationDestinations() *NotificationDestinationRepository {
	return &NotificationDestinationRepository{s: s}
}

const destinationColumns = `server_id,user_id,kind,topic,url,format,enabled,created_at,updated_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanDestination(row rowScanner) (NotificationDestination, error) {
	var value NotificationDestination
	var created, updated string
	var err = row.Scan(&value.ServerID, &value.UserID, &value.Kind, &value.Topic, &value.URL, &value.Format, &value.Enabled, &created, &updated)

	if err != nil {
		return value, err
	}
	if value.CreatedAt, err = ParseTime(created); err != nil {
		return value, err
	}
	value.UpdatedAt, err = ParseTime(updated)
	return value, err
}

// Get returns the user's destination, or ErrNotFound.
func (r *NotificationDestinationRepository) Get(ctx context.Context, serverID, userID string) (NotificationDestination, error) {
	var value, err = scanDestination(r.s.ro.QueryRowContext(ctx, `SELECT `+destinationColumns+` FROM notification_destinations WHERE server_id=? AND user_id=?`, serverID, userID))

	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, fmt.Errorf("get notification destination: %w", err)
	}
	return value, nil
}

// Put creates or replaces the user's destination, keeping its creation time.
func (r *NotificationDestinationRepository) Put(ctx context.Context, value NotificationDestination) error {
	var now = FormatTime(r.s.nowUTC())
	var _, err = r.s.rw.ExecContext(ctx, `INSERT INTO notification_destinations(`+destinationColumns+`) VALUES(?,?,?,?,?,?,?,?,?)
ON CONFLICT(server_id,user_id) DO UPDATE SET kind=excluded.kind,topic=excluded.topic,url=excluded.url,format=excluded.format,enabled=excluded.enabled,updated_at=excluded.updated_at`,
		value.ServerID, value.UserID, value.Kind, value.Topic, value.URL, value.Format, value.Enabled, now, now)

	if err != nil {
		return fmt.Errorf("save notification destination: %w", err)
	}
	return nil
}

// Delete removes the user's destination. It reports ErrNotFound when none exists.
func (r *NotificationDestinationRepository) Delete(ctx context.Context, serverID, userID string) error {
	var result, err = r.s.rw.ExecContext(ctx, `DELETE FROM notification_destinations WHERE server_id=? AND user_id=?`, serverID, userID)

	if err != nil {
		return fmt.Errorf("delete notification destination: %w", err)
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

// LastUserDelivery returns the newest per-user outbox row among the user's
// requests, or ErrNotFound when nothing was ever queued.
func (r *NotificationDestinationRepository) LastUserDelivery(ctx context.Context, serverID, userID string) (UserDelivery, error) {
	var value UserDelivery
	var delivered sql.NullString
	var err = r.s.ro.QueryRowContext(ctx, `SELECT o.delivered_at,o.last_error,o.attempts FROM user_availability_outbox o JOIN media_requests mr ON mr.id=o.request_id
WHERE mr.server_id=? AND mr.user_id=? ORDER BY o.id DESC LIMIT 1`, serverID, userID).Scan(&delivered, &value.LastError, &value.Attempts)

	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, fmt.Errorf("last user delivery: %w", err)
	}
	value.DeliveredAt, err = scanNullTime(delivered)
	return value, err
}
