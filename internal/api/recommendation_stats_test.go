package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func TestRecommendationStatsEndpoint(t *testing.T) {
	srv, st := newTestServer(t, nil)
	h := srv.Handler()
	ctx := context.Background()
	offered := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

	if err := st.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: "user", DisplayName: "User", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().ReplaceOffered(ctx, "server", "user", "movie", []model.Recommendation{
		{TMDBID: 1, Title: "One", Score: 90, Reasons: []string{"reason"}, GeneratedAt: offered, ExpiresAt: offered.Add(time.Hour)},
		{TMDBID: 2, Title: "Two", Score: 80, Reasons: []string{"reason"}, GeneratedAt: offered, ExpiresAt: offered.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	values, err := st.Recommendations().List(ctx, "server", "user", "movie", "active", 10, 0)
	if err != nil || len(values) != 2 {
		t.Fatalf("values=%+v err=%v", values, err)
	}
	if _, _, err := st.Recommendations().RecordAction(ctx, values[0].ID, "request-1", "request"); err != nil {
		t.Fatal(err)
	}

	rec := do(t, h, http.MethodGet, "/api/v1/recommendations/stats?server_id=server&user_id=user", authed())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"offered": 2.0, "requested": 1.0, "request_rate": 0.5, "dismissed": 0.0, "rated": 0.0, "average_rating": 0.0,
		"available": 0.0, "watched": 0.0, "completed": 0.0, "days": 90.0, "measuring_since": "2026-08-20T09:00:00Z", "since": "2026-05-25T12:00:00Z"}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
	t.Log("\n" + rec.Body.String())

	if rec := do(t, h, http.MethodGet, "/api/v1/recommendations/stats?server_id=server&user_id=user&days=7", authed()); rec.Code != http.StatusOK {
		t.Errorf("days=7 status = %d", rec.Code)
	}
	for _, query := range []string{"days=6", "days=366", "days=0", "days=abc", "days=-30"} {
		if rec := do(t, h, http.MethodGet, "/api/v1/recommendations/stats?server_id=server&user_id=user&"+query, authed()); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", query, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodGet, "/api/v1/recommendations/stats?server_id=server", authed()); rec.Code != http.StatusBadRequest {
		t.Errorf("missing user status = %d, want 400", rec.Code)
	}
}
