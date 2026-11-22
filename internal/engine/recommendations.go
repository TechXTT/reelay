package engine

import (
	"context"
	"errors"

	"github.com/TechXTT/reelay/internal/store"
)

// RecommendationsOnce regenerates every enabled user's recommendations and
// tracks TMDB the way MetadataOnce tracks TVmaze: one failing generation is a
// title or user problem, every generation failing at the provider is an
// outage. Interactive generation never records incidents; its caller gets the
// error.
func (e *Engine) RecommendationsOnce(ctx context.Context) error {
	report, err := e.recommendations.GenerateAll(ctx)
	if report.Generated > 0 {
		e.incidentHealthy(store.IncidentMetadata, TMDBIncidentKey)
	} else if report.Failure != nil {
		e.incidentFailed(store.IncidentMetadata, TMDBIncidentKey, report.Failure.Error(), report.Failed...)
	}
	return errors.Join(err, e.flushIncidents(context.WithoutCancel(ctx), store.IncidentMetadata))
}
