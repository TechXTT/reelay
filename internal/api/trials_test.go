package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
)

func TestTrialRequestsRequireWatchedEpisodesAndVoteBeforeWidening(t *testing.T) {
	var ctx = context.Background()
	var srv, db = newTestServer(t, func(cfg *config.Config) { cfg.Library.TVRoot = t.TempDir() })

	srv.discovery, srv.externalSeries = requestDiscoveryStub{}, requestSeriesStub{}
	addRequestUser(t, db, "server", "user")
	seedRequestProfile(t, db)
	if err := db.Recommendations().Replace(ctx, "server", "user", "series", []model.Recommendation{{TMDBID: 72, Title: "Trial", GeneratedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	recommendations, err := db.Recommendations().List(ctx, "server", "user", "series", "active", 10, 0)
	if err != nil || len(recommendations) != 1 {
		t.Fatalf("missing fixture: %+v %v", recommendations, err)
	}
	var path = fmt.Sprintf("/api/v1/recommendations/%d/actions", recommendations[0].ID)
	if response := doJSON(t, srv.Handler(), http.MethodPost, path, map[string]any{"action_id": "trial", "action": "request", "trial": "three_episodes"}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if response := doJSON(t, srv.Handler(), http.MethodPost, path, map[string]any{"action_id": "bypass", "action": "request", "monitor_mode": "all"}); response.Code != 409 {
		t.Fatalf("trial bypass: %d %s", response.Code, response.Body.String())
	}
	request, err := db.Requests().TrialForTitle(ctx, "server", "user", 72)
	if err != nil {
		t.Fatal(err)
	}
	series, err := db.Series().GetByTMDBID(ctx, 72)
	if err != nil {
		t.Fatal(err)
	}
	for number := 1; number <= 3; number++ {
		if _, _, err := db.Episodes().UpsertMetadata(ctx, model.Episode{SeriesID: series.ID, Season: 1, Number: number}, model.StateImported, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Requests().PrepareTrialEpisodes(ctx, series.ID); err != nil {
		t.Fatal(err)
	}
	votePath := fmt.Sprintf("/api/v1/trials/%d/vote", request.RequestID)
	if response := doJSON(t, srv.Handler(), http.MethodPost, votePath, map[string]any{"decision": "continue", "rating": 5}); response.Code != 409 {
		t.Fatalf("premature vote: %d", response.Code)
	}
	var events = []map[string]any{{"request_id": request.RequestID, "season": 1, "number": 1}, {"request_id": request.RequestID, "season": 1, "number": 2}, {"request_id": request.RequestID, "season": 1, "number": 3}}
	if response := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/integrations/jellyfin/trial-playback", map[string]any{"server_id": "server", "user_id": "user", "episodes": events}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if response := doJSON(t, srv.Handler(), http.MethodPost, votePath, map[string]any{"decision": "continue"}); response.Code != 400 {
		t.Fatalf("missing rating accepted: %d", response.Code)
	}
	if response := doJSON(t, srv.Handler(), http.MethodPost, votePath, map[string]any{"decision": "continue", "rating": 4}); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if response := doJSON(t, srv.Handler(), http.MethodPost, path, map[string]any{"action_id": "trial", "action": "request", "trial": "three_episodes"}); response.Code != 200 {
		t.Fatalf("trial action replay failed: %s", response.Body.String())
	}
	series, err = db.Series().Get(ctx, series.ID)
	if err != nil || series.MonitorMode != model.MonitorAll {
		t.Fatalf("positive vote did not continue: %+v %v", series, err)
	}
}
