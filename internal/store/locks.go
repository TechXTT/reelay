package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

type ItemLocker struct{ s *Store }

func (s *Store) Locks() *ItemLocker { return &ItemLocker{s: s} }

type ItemLock struct {
	s       *Store
	Subject model.SubjectType
	ID      int64
	Owner   string
	Expires time.Time
}

func (l *ItemLocker) Acquire(ctx context.Context, subject model.SubjectType, id int64, owner string, ttl time.Duration) (*ItemLock, error) {
	if !subject.ValidItem() || id <= 0 {
		return nil, fmt.Errorf("lock: invalid subject %q:%d", subject, id)
	}
	if owner == "" {
		owner = randomOwner()
	}
	if ttl < time.Second {
		ttl = 30 * time.Second
	}
	now := l.s.nowUTC()
	expires := now.Add(ttl).UTC().Truncate(time.Second)
	acquired := false
	err := l.s.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM item_locks
 WHERE subject_type = ? AND subject_id = ? AND expires_at <= ?`,
			subject, id, FormatTime(now)); err != nil {
			return fmt.Errorf("remove expired item lock: %w", err)
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO item_locks
 (subject_type, subject_id, owner, acquired_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
			subject, id, owner, FormatTime(now), FormatTime(expires))
		if err != nil {
			return fmt.Errorf("acquire item lock: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("acquire item lock result: %w", err)
		}
		acquired = n == 1
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !acquired {
		return nil, fmt.Errorf("%s:%d: %w", subject, id, ErrLocked)
	}
	return &ItemLock{s: l.s, Subject: subject, ID: id, Owner: owner, Expires: expires}, nil
}

func (l *ItemLock) Renew(ctx context.Context, ttl time.Duration) error {
	if ttl < time.Second {
		ttl = 30 * time.Second
	}
	now := l.s.nowUTC()
	expires := now.Add(ttl).UTC().Truncate(time.Second)
	res, err := l.s.rw.ExecContext(ctx, `UPDATE item_locks SET expires_at = ?
 WHERE subject_type = ? AND subject_id = ? AND owner = ? AND expires_at > ?`,
		FormatTime(expires), l.Subject, l.ID, l.Owner, FormatTime(now))
	if err != nil {
		return fmt.Errorf("renew item lock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("renew %s:%d: %w", l.Subject, l.ID, ErrLocked)
	}
	l.Expires = expires
	return nil
}

func (l *ItemLock) Release(ctx context.Context) error {
	res, err := l.s.rw.ExecContext(ctx, `DELETE FROM item_locks
 WHERE subject_type = ? AND subject_id = ? AND owner = ?`, l.Subject, l.ID, l.Owner)
	if err != nil {
		return fmt.Errorf("release item lock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("release %s:%d: %w", l.Subject, l.ID, ErrLocked)
	}
	return nil
}

func randomOwner() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("process-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
