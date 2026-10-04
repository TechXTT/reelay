package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
)

func TestSeasonRequestValidationAndWithdrawalReplay(t *testing.T) {
	ctx := context.Background()
	srv, st := newTestServer(t, func(cfg *config.Config) { cfg.Library.TVRoot = t.TempDir() })
	srv.discovery, srv.externalSeries = requestDiscoveryStub{}, requestSeriesStub{}
	addRequestUser(t, st, "server", "user")
	seedRequestProfile(t, st)
	now := time.Now()
	if err := st.Recommendations().Replace(ctx, "server", "user", "series", []model.Recommendation{{TMDBID: 1, Title: "Selected", GeneratedAt: now, ExpiresAt: now.Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	recs, err := st.Recommendations().List(ctx, "server", "user", "series", "active", 10, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recommendations %v %v", recs, err)
	}
	path := "/api/v1/recommendations/" + strconv.FormatInt(recs[0].ID, 10) + "/actions"
	for _, body := range []map[string]any{
		{"action_id": "bad", "action": "request", "seasons": []int{-1}},
		{"action_id": "bad", "action": "request", "seasons": []int{1}, "monitor_mode": "all"},
		{"action_id": "bad", "action": "dismiss", "seasons": []int{1}},
	} {
		if response := doJSON(t, srv.Handler(), http.MethodPost, path, body); response.Code != 400 {
			t.Fatalf("invalid scope status=%d body=%s", response.Code, response.Body.String())
		}
	}
	body := map[string]any{"action_id": "first", "action": "request", "seasons": []int{0, 2, 2}}
	if response := doJSON(t, srv.Handler(), http.MethodPost, path, body); response.Code != 200 {
		t.Fatalf("request: %s", response.Body.String())
	}
	requests, err := st.Requests().List(ctx, "server", "user")
	if err != nil || len(requests) != 1 || len(requests[0].Seasons) != 2 {
		t.Fatalf("requests=%+v %v", requests, err)
	}
	series, err := st.Series().Get(ctx, requests[0].SubjectID)
	if err != nil || series.MonitorMode != model.MonitorNone {
		t.Fatalf("scope widened to %s: %v", series.MonitorMode, err)
	}
	requestPath := "/api/v1/requests/" + strconv.FormatInt(requests[0].ID, 10) + "/actions"
	if response := doJSON(t, srv.Handler(), http.MethodPost, requestPath, map[string]string{"action": "cancel"}); response.Code != 200 {
		t.Fatalf("withdraw: %s", response.Body.String())
	}
	if response := doJSON(t, srv.Handler(), http.MethodPost, path, body); response.Code != 200 {
		t.Fatalf("replay: %s", response.Body.String())
	}
	request, err := st.Requests().Get(ctx, requests[0].ID)
	if err != nil || request.CancelledAt == nil {
		t.Fatalf("replay reopened withdrawal: %+v %v", request, err)
	}
	body["action_id"] = "new-request"
	if response := doJSON(t, srv.Handler(), http.MethodPost, path, body); response.Code != 200 {
		t.Fatalf("new request: %s", response.Body.String())
	}
	request, err = st.Requests().Get(ctx, requests[0].ID)
	if err != nil || request.CancelledAt != nil {
		t.Fatalf("new request not reactivated: %+v %v", request, err)
	}
	if response := doJSON(t, srv.Handler(), http.MethodPost, path, map[string]string{"action_id": "future", "action": "request", "monitor_mode": "future_only"}); response.Code != 200 {
		t.Fatalf("future scope: %s", response.Body.String())
	}
	series, err = st.Series().Get(ctx, series.ID)
	if err != nil || series.MonitorMode != model.MonitorFutureOnly {
		t.Fatalf("future scope did not widen: %+v %v", series, err)
	}
}

func TestRequestsPlayableLinkAndInvalidOffset(t *testing.T) {
	ctx := context.Background()
	srv, st := newTestServer(t, func(cfg *config.Config) {
		cfg.Availability.JellyfinServers = map[string]string{"plugin-server": "http://jellyfin.invalid/jellyfin"}
	})
	addRequestUser(t, st, "plugin-server", "user")
	profile := seedRequestProfile(t, st)
	movie, err := st.Movies().Create(ctx, model.Movie{Title: "Available", TMDBID: 1, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Requests().Create(ctx, model.MediaRequest{ServerID: "plugin-server", UserID: "user", MediaType: "movie", TMDBID: 1, Title: "Available", SubjectType: "movie", SubjectID: movie.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().UpsertItems(ctx, []model.JellyfinItem{{ServerID: "plugin-server", ItemID: "playable", MediaType: "movie", TMDBID: 1, Title: "Available", Present: true}}, "sync"); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/requests?server_id=plugin-server&user_id=user"
	response := do(t, srv.Handler(), http.MethodGet, path, authed())
	var result struct {
		Items []model.MediaRequest `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || len(result.Items) != 1 {
		t.Fatalf("response %s: %v", response.Body.String(), err)
	}
	if result.Items[0].JellyfinURL != "http://jellyfin.invalid/jellyfin/web/index.html#!/details?id=playable" {
		t.Fatalf("incorrect playable link %q", result.Items[0].JellyfinURL)
	}
	for _, offset := range []string{"nope", "-1", "1000001"} {
		if response := do(t, srv.Handler(), http.MethodGet, path+"&offset="+offset, authed()); response.Code != 400 {
			t.Fatalf("offset %q accepted", offset)
		}
	}
}
