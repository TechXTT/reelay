package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
)

type recommendationMovieProvider struct{}

func (recommendationMovieProvider) SearchMovies(_ context.Context, _ string, _ int) ([]metadata.Movie, error) {
	return nil, nil
}

func (recommendationMovieProvider) MovieDetails(_ context.Context, id int) (metadata.Movie, error) {
	return metadata.Movie{TMDBID: id, Title: "Candidate", Year: 2024, RuntimeMinutes: 120}, nil
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	for key, value := range authed() {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRecommendationRequestCreatesMovieIdempotently(t *testing.T) {
	root := t.TempDir()
	srv, st := newTestServer(t, func(cfg *config.Config) { cfg.Library.MovieRoot = root })
	srv.movies = recommendationMovieProvider{}
	if _, err := st.Profiles().Seed(context.Background(), []model.QualityProfile{{Name: "Default", IsDefault: true, AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"}, MinSeeders: 1}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if err := st.Recommendations().UpsertUser(context.Background(), model.JellyfinUser{ServerID: "server", UserID: "user", DisplayName: "User", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().Replace(context.Background(), "server", "user", "movie", []model.Recommendation{{TMDBID: 99, Title: "Candidate", Score: 80, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	values, err := st.Recommendations().List(context.Background(), "server", "user", "movie", "active", 10, 0)
	if err != nil || len(values) != 1 {
		t.Fatalf("values=%+v err=%v", values, err)
	}
	path := "/api/v1/recommendations/" + strconv.FormatInt(values[0].ID, 10) + "/actions"
	for i := 0; i < 2; i++ {
		response := doJSON(t, srv.Handler(), http.MethodPost, path, map[string]string{"action_id": "request-1", "action": "request"})
		if response.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, response.Code, response.Body.String())
		}
	}
	movies, err := st.Movies().List(context.Background())
	if err != nil || len(movies) != 1 || movies[0].TMDBID != 99 || movies[0].State != model.StateWanted {
		t.Fatalf("movies=%+v err=%v", movies, err)
	}
}

func TestJellyfinSyncEventsAndRecommendationDismissal(t *testing.T) {
	srv, st := newTestServer(t, nil)
	handler := srv.Handler()
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	sync := map[string]any{"server_id": "server", "sync_token": "sync-1", "complete": true, "users": []model.JellyfinUser{{ServerID: "server", UserID: "user", DisplayName: "User", Enabled: true}}, "items": []model.JellyfinItem{{ServerID: "server", ItemID: "item", MediaType: "movie", TMDBID: 42, Title: "Arrival", Present: true}}}
	if rec := doJSON(t, handler, http.MethodPost, "/api/v1/integrations/jellyfin/sync", sync); rec.Code != http.StatusOK {
		t.Fatalf("sync status=%d body=%s", rec.Code, rec.Body.String())
	}
	events := map[string]any{"events": []model.JellyfinActivity{{EventID: "event", ServerID: "server", UserID: "user", ItemID: "item", EventType: "completed", Progress: 1, OccurredAt: now}, {EventID: "event", ServerID: "server", UserID: "user", ItemID: "item", EventType: "completed", Progress: 1, OccurredAt: now}}}
	if rec := doJSON(t, handler, http.MethodPost, "/api/v1/integrations/jellyfin/events", events); rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"duplicates":1`)) {
		t.Fatalf("events status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := st.Recommendations().Replace(context.Background(), "server", "user", "movie", []model.Recommendation{{TMDBID: 99, Title: "Candidate", Score: 80, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	list := do(t, handler, http.MethodGet, "/api/v1/recommendations?server_id=server&user_id=user&media_type=movie", authed())
	if list.Code != http.StatusOK {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}
	var payload struct {
		Items []model.Recommendation `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &payload); err != nil || len(payload.Items) != 1 {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}
	action := doJSON(t, handler, http.MethodPost, "/api/v1/recommendations/"+strconv.FormatInt(payload.Items[0].ID, 10)+"/actions", map[string]string{"action_id": "dismiss-1", "action": "dismiss"})
	if action.Code != http.StatusOK {
		t.Fatalf("action=%d %s", action.Code, action.Body.String())
	}
	empty := do(t, handler, http.MethodGet, "/api/v1/recommendations?server_id=server&user_id=user&media_type=movie", authed())
	if !bytes.Contains(empty.Body.Bytes(), []byte(`"items":null`)) {
		t.Fatalf("active list=%s", empty.Body.String())
	}
}
