package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// touchInterval bounds how often a session's last_seen_at is rewritten, so a
// busy dashboard does not turn every request into a database write.
const touchInterval = time.Minute

// Session is a stored login. The token itself is never stored, only its hash.
type Session struct {
	TokenHash   string
	Role        string
	ServerID    string
	UserID      string
	DisplayName string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastSeenAt  time.Time
}

type SessionRepository struct{ s *Store }

func (s *Store) Sessions() *SessionRepository { return &SessionRepository{s: s} }

func (r *SessionRepository) Create(ctx context.Context, value Session) error {
	_, err := r.s.rw.ExecContext(ctx, `INSERT INTO sessions(token_hash,role,server_id,user_id,display_name,created_at,expires_at,last_seen_at)
VALUES(?,?,?,?,?,?,?,?)`, value.TokenHash, value.Role, value.ServerID, value.UserID, value.DisplayName,
		FormatTime(value.CreatedAt), FormatTime(value.ExpiresAt), FormatTime(value.LastSeenAt))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Get returns the session for tokenHash, or ErrNotFound when it is unknown or
// expired at now.
func (r *SessionRepository) Get(ctx context.Context, tokenHash string, now time.Time) (Session, error) {
	var value = Session{TokenHash: tokenHash}
	var created, expires, seen string
	var err = r.s.ro.QueryRowContext(ctx, `SELECT role,server_id,user_id,display_name,created_at,expires_at,last_seen_at
FROM sessions WHERE token_hash=? AND expires_at>?`, tokenHash, FormatTime(now)).Scan(
		&value.Role, &value.ServerID, &value.UserID, &value.DisplayName, &created, &expires, &seen)

	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	if err != nil {
		return value, fmt.Errorf("get session: %w", err)
	}
	for target, raw := range map[*time.Time]string{&value.CreatedAt: created, &value.ExpiresAt: expires, &value.LastSeenAt: seen} {
		if *target, err = ParseTime(raw); err != nil {
			return value, err
		}
	}
	return value, nil
}

// Touch records activity, skipping the write when last_seen_at is already
// within a minute of now.
func (r *SessionRepository) Touch(ctx context.Context, tokenHash string, now time.Time) error {
	_, err := r.s.rw.ExecContext(ctx, `UPDATE sessions SET last_seen_at=? WHERE token_hash=? AND last_seen_at<?`,
		FormatTime(now), tokenHash, FormatTime(now.Add(-touchInterval)))
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

func (r *SessionRepository) Delete(ctx context.Context, tokenHash string) error {
	_, err := r.s.rw.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpired removes sessions that expired at or before now and returns how
// many were removed.
func (r *SessionRepository) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.s.rw.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, FormatTime(now))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return res.RowsAffected()
}
