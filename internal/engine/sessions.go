package engine

import "context"

// PruneSessionsOnce deletes expired login sessions. Expired sessions are
// already refused on lookup, so this only bounds table growth. Called on
// startup and daily while auth.logins_enabled is true.
func (e *Engine) PruneSessionsOnce(ctx context.Context) error {
	var deleted int64
	var err error

	deleted, err = e.store.Sessions().DeleteExpired(ctx, e.clock.Now())
	if err != nil {
		return err
	}
	if deleted > 0 {
		e.log.Info("expired sessions pruned", "deleted", deleted)
	}
	return nil
}
