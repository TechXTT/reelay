package engine

import (
	"context"
)

// PruneAuditOnce deletes state transitions older than runtime.audit_retention,
// keeping each item's latest transition. Called on startup and daily.
func (e *Engine) PruneAuditOnce(ctx context.Context) error {
	var cutoff = e.clock.Now().UTC().Add(-e.cfg.Runtime.AuditRetention.Duration)
	var deleted int64
	var err error

	deleted, err = e.store.Transitions().PruneBefore(ctx, cutoff)
	if err != nil {
		return err
	}
	if deleted > 0 {
		e.log.Info("audit history pruned", "deleted", deleted, "cutoff", cutoff)
	}
	return nil
}
