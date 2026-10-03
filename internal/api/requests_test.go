package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type requestDiscoveryStub struct{}

func (requestDiscoveryStub) Recommendations(context.Context, string, int) ([]metadata.DiscoveryItem, error) {
	return nil, nil
}
func (requestDiscoveryStub) Similar(context.Context, string, int) ([]metadata.DiscoveryItem, error) {
	return nil, nil
}
func (requestDiscoveryStub) Discover(context.Context, string) ([]metadata.DiscoveryItem, error) {
	return nil, nil
}
func (requestDiscoveryStub) DiscoveryDetails(_ context.Context, mediaType string, tmdbID int) (metadata.DiscoveryItem, error) {
	return metadata.DiscoveryItem{MediaType: mediaType, TMDBID: tmdbID, TVDBID: tmdbID}, nil
}

type requestSeriesStub struct{}

func (requestSeriesStub) SearchSeries(context.Context, string) ([]metadata.Series, error) {
	return nil, nil
}
func (requestSeriesStub) SeriesEpisodes(context.Context, int) ([]metadata.Episode, error) {
	return nil, nil
}
func (requestSeriesStub) LookupSeries(_ context.Context, tvdbID int, imdbID string) (metadata.Series, error) {
	return metadata.Series{TVmazeID: tvdbID, Title: "Requested Series", IMDBID: imdbID}, nil
}

func addRequestUser(t *testing.T, st *store.Store, serverID, userID string) {
	t.Helper()
	if err := st.Recommendations().UpsertUser(context.Background(), model.JellyfinUser{ServerID: serverID, UserID: userID, DisplayName: userID, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func seedRequestProfile(t *testing.T, st *store.Store) model.QualityProfile {
	t.Helper()
	_, err := st.Profiles().Seed(context.Background(), []model.QualityProfile{{Name: "Default", IsDefault: true, AllowedResolutions: []string{"1080p"}, AllowedSources: []string{"webdl"}, MinSeeders: 1}})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := st.Profiles().Default(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestRequestTrackingSeparatesAttributionFromJellyfinAvailability(t *testing.T) {
	srv, st := newTestServer(t, nil)
	ctx := context.Background()
	addRequestUser(t, st, "server", "alice")
	addRequestUser(t, st, "server", "bob")
	profile := seedRequestProfile(t, st)
	movie, err := st.Movies().Create(ctx, model.Movie{Title: "Candidate", Year: 2024, TMDBID: 99, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "request fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Writer().ExecContext(ctx, `UPDATE movies SET state='imported' WHERE id=?`, movie.ID); err != nil {
		t.Fatal(err)
	}
	for _, userID := range []string{"alice", "bob"} {
		_, err := st.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: userID, MediaType: "movie", TMDBID: 99, Title: "Candidate", Year: 2024, RequestedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), SubjectType: "movie", SubjectID: movie.ID})
		if err != nil {
			t.Fatalf("create request for %s: %v", userID, err)
		}
	}

	path := "/api/v1/requests?server_id=server&user_id=alice"
	response := do(t, srv.Handler(), http.MethodGet, path, authed())
	if response.Code != http.StatusOK {
		t.Fatalf("requests status=%d body=%s", response.Code, response.Body.String())
	}
	var got struct {
		Items []model.MediaRequest `json:"items"`
		Limit int                  `json:"limit"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || len(got.Items) != 1 || got.Limit != model.MediaRequestListLimit {
		t.Fatalf("requests=%+v err=%v", got, err)
	}
	if got.Items[0].State != "imported" || got.Items[0].Progress != 1 || got.Items[0].Available {
		t.Fatalf("before Jellyfin sync request=%+v", got.Items[0])
	}

	if err := st.Recommendations().UpsertItems(ctx, []model.JellyfinItem{{ServerID: "server", ItemID: "jellyfin-candidate", MediaType: "movie", TMDBID: 99, Title: "Candidate", Present: true}}, "sync-1"); err != nil {
		t.Fatal(err)
	}
	response = do(t, srv.Handler(), http.MethodGet, path, authed())
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || len(got.Items) != 1 || !got.Items[0].Available {
		t.Fatalf("after Jellyfin sync requests=%+v err=%v", got, err)
	}
	response = do(t, srv.Handler(), http.MethodGet, "/api/v1/requests?server_id=server&user_id=unknown", authed())
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || len(got.Items) != 0 {
		t.Fatalf("other user requests=%+v err=%v", got, err)
	}
	if err := st.Movies().Delete(ctx, movie.ID); err != nil {
		t.Fatal(err)
	}
	unrelated, err := st.Movies().Create(ctx, model.Movie{Title: "Unrelated", Year: 2024, TMDBID: 100, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "row id reuse fixture")
	if err != nil || unrelated.ID != movie.ID {
		t.Fatalf("reused subject id=%d, original=%d err=%v", unrelated.ID, movie.ID, err)
	}
	_, err = st.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "alice", MediaType: "movie", TMDBID: 100, Title: "Unrelated", Year: 2024, SubjectType: "movie", SubjectID: unrelated.ID})
	if err != nil {
		t.Fatal(err)
	}
	response = do(t, srv.Handler(), http.MethodGet, path, authed())
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || len(got.Items) != 2 {
		t.Fatalf("reused row id should not claim another title: requests=%+v err=%v", got, err)
	}
	for _, request := range got.Items {
		if request.TMDBID == 99 && (request.State != "missing" || request.Progress != 0 || request.ImportedEpisodes != 0) {
			t.Fatalf("reused row id leaked another title's summary: %+v", request)
		}
	}
	recreated, err := st.Movies().Create(ctx, model.Movie{Title: "Candidate", Year: 2024, TMDBID: 99, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "re-request fixture")
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "alice", MediaType: "movie", TMDBID: 99, Title: "Candidate", Year: 2024, SubjectType: "movie", SubjectID: recreated.ID, MonitorMode: ""})
	if err != nil {
		t.Fatal(err)
	}
	response = do(t, srv.Handler(), http.MethodGet, path, authed())
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || len(got.Items) != 2 {
		t.Fatalf("re-request did not relink canonical subject: requests=%+v err=%v", got, err)
	}
	for _, request := range got.Items {
		if request.TMDBID == 99 && (request.State != string(model.StateWanted) || request.SubjectID != recreated.ID) {
			t.Fatalf("re-request did not relink canonical subject: %+v", request)
		}
	}
}

func TestSeriesRequestScopeDefaultsAndWidensWithoutNarrowing(t *testing.T) {
	root := t.TempDir()
	srv, st := newTestServer(t, func(cfg *config.Config) { cfg.Library.TVRoot = root })
	srv.discovery = requestDiscoveryStub{}
	srv.externalSeries = requestSeriesStub{}
	ctx := context.Background()
	addRequestUser(t, st, "server", "user")
	profile := seedRequestProfile(t, st)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	recommendations := []model.Recommendation{
		{TMDBID: 101, Title: "New", Score: 80, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)},
		{TMDBID: 102, Title: "Current", Score: 70, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)},
		{TMDBID: 103, Title: "Complete", Score: 60, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	if err := st.Recommendations().Replace(ctx, "server", "user", "series", recommendations); err != nil {
		t.Fatal(err)
	}
	for _, tmdbID := range []int{102, 103} {
		mode := model.MonitorFutureOnly
		if tmdbID == 103 {
			mode = model.MonitorAll
		}
		_, err := st.Series().Create(ctx, model.Series{Title: "Existing", Year: 2020, TVmazeID: tmdbID, TMDBID: tmdbID, MonitorMode: mode, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: root})
		if err != nil {
			t.Fatal(err)
		}
	}
	values, err := st.Recommendations().List(ctx, "server", "user", "series", "active", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	byTMDB := map[int]model.Recommendation{}
	for _, value := range values {
		byTMDB[value.TMDBID] = value
	}
	for _, tc := range []struct {
		tmdbID int
		mode   string
	}{
		{101, "invalid"},
		{101, ""},
		{102, string(model.MonitorLatestSeason)},
		{103, string(model.MonitorLatestSeason)},
	} {
		path := "/api/v1/recommendations/" + strconv.FormatInt(byTMDB[tc.tmdbID].ID, 10) + "/actions"
		if tc.mode == "invalid" {
			response := doJSON(t, srv.Handler(), http.MethodPost, path, map[string]string{"action_id": "invalid-scope", "action": "request", "monitor_mode": tc.mode})
			if response.Code != http.StatusBadRequest {
				t.Fatalf("invalid scope status=%d body=%s", response.Code, response.Body.String())
			}
			continue
		}
		request := map[string]string{"action_id": "request-" + strconv.Itoa(tc.tmdbID), "action": "request"}
		if tc.mode != "" {
			request["monitor_mode"] = tc.mode
		}
		response := doJSON(t, srv.Handler(), http.MethodPost, path, request)
		if response.Code != http.StatusOK {
			t.Fatalf("request tmdb=%d status=%d body=%s", tc.tmdbID, response.Code, response.Body.String())
		}
	}
	replayPath := "/api/v1/recommendations/" + strconv.FormatInt(byTMDB[101].ID, 10) + "/actions"
	replay := doJSON(t, srv.Handler(), http.MethodPost, replayPath, map[string]string{"action_id": "request-101", "action": "request", "monitor_mode": string(model.MonitorAll)})
	if replay.Code != http.StatusOK {
		t.Fatalf("request replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	for tmdbID, want := range map[int]model.MonitorMode{101: model.MonitorFutureOnly, 102: model.MonitorLatestSeason, 103: model.MonitorAll} {
		series, err := st.Series().GetByTMDBID(ctx, tmdbID)
		if err != nil || series.MonitorMode != want {
			t.Fatalf("series tmdb=%d mode=%s err=%v, want %s", tmdbID, series.MonitorMode, err, want)
		}
	}
	requests, err := st.Requests().List(ctx, "server", "user")
	if err != nil || len(requests) != 3 {
		t.Fatalf("requests=%+v err=%v", requests, err)
	}
	for _, request := range requests {
		if request.MonitorMode == "" {
			t.Fatalf("request scope should record the effective requested default: %+v", request)
		}
		if request.TMDBID == 101 && request.MonitorMode != string(model.MonitorFutureOnly) {
			t.Fatalf("replayed action changed stored scope: %+v", request)
		}
	}
}

func TestRequestActionReplayCannotBeReusedForAnotherTitle(t *testing.T) {
	root := t.TempDir()
	srv, st := newTestServer(t, func(cfg *config.Config) { cfg.Library.MovieRoot = root })
	srv.movies = recommendationMovieProvider{}
	_ = seedRequestProfile(t, st)
	addRequestUser(t, st, "server", "user")
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := st.Recommendations().Replace(context.Background(), "server", "user", "movie", []model.Recommendation{
		{TMDBID: 201, Title: "First", Score: 80, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)},
		{TMDBID: 202, Title: "Second", Score: 70, Reasons: []string{"reason"}, GeneratedAt: now, ExpiresAt: now.Add(time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	values, err := st.Recommendations().List(context.Background(), "server", "user", "movie", "active", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		response := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/recommendations/"+strconv.FormatInt(value.ID, 10)+"/actions", map[string]string{"action_id": "same-action", "action": "request"})
		if value.TMDBID == 201 && response.Code != http.StatusOK {
			t.Fatalf("first action status=%d body=%s", response.Code, response.Body.String())
		}
		if value.TMDBID == 202 && response.Code == http.StatusOK {
			t.Fatalf("reused action id unexpectedly succeeded: %s", response.Body.String())
		}
	}
	movies, err := st.Movies().List(context.Background())
	if err != nil || len(movies) != 1 || movies[0].TMDBID != 201 {
		t.Fatalf("movies after mismatched replay=%+v err=%v", movies, err)
	}
}

func TestSeriesRequestSummaryPrioritizesAttentionAndNamesUnmonitoredEpisodes(t *testing.T) {
	_, st := newTestServer(t, nil)
	ctx := context.Background()
	addRequestUser(t, st, "server", "user")
	profile := seedRequestProfile(t, st)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	series, err := st.Series().Create(ctx, model.Series{Title: "Tracked", TMDBID: 301, MonitorMode: model.MonitorAll, Status: model.SeriesFollowing, ProfileID: profile.ID, RootFolder: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range []model.Episode{
		{SeriesID: series.ID, Season: 1, Number: 1, State: model.StateImported},
		{SeriesID: series.ID, Season: 1, Number: 2, State: model.StateImportFailed, LastError: "import failed"},
		{SeriesID: series.ID, Season: 1, Number: 3, State: model.StateDownloading},
	} {
		if _, err := st.Episodes().Create(ctx, ep, "summary fixture"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = st.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "series", TMDBID: 301, Title: "Tracked", RequestedAt: now, SubjectType: "series", SubjectID: series.ID, MonitorMode: string(model.MonitorAll)})
	if err != nil {
		t.Fatal(err)
	}
	requests, err := st.Requests().List(ctx, "server", "user")
	if err != nil || len(requests) != 1 || requests[0].State != "attention_needed" || requests[0].ImportedEpisodes != 1 || requests[0].TotalEpisodes != 3 {
		t.Fatalf("failure summary=%+v err=%v", requests, err)
	}
	if _, err := st.Writer().ExecContext(ctx, `UPDATE episodes SET state='imported' WHERE series_id=? AND number=2`, series.ID); err != nil {
		t.Fatal(err)
	}
	requests, err = st.Requests().List(ctx, "server", "user")
	if err != nil || len(requests) != 1 || requests[0].State != "downloading" || requests[0].ImportedEpisodes != 2 {
		t.Fatalf("active summary=%+v err=%v", requests, err)
	}
	if _, err := st.Writer().ExecContext(ctx, `UPDATE episodes SET state='unmonitored' WHERE series_id=?`, series.ID); err != nil {
		t.Fatal(err)
	}
	requests, err = st.Requests().List(ctx, "server", "user")
	if err != nil || len(requests) != 1 || requests[0].State != "not_monitored" {
		t.Fatalf("unmonitored summary=%+v err=%v", requests, err)
	}
	if _, err := st.Writer().ExecContext(ctx, `UPDATE episodes SET air_date=? WHERE series_id=?`, "2027-01-01T00:00:00Z", series.ID); err != nil {
		t.Fatal(err)
	}
	requests, err = st.Requests().List(ctx, "server", "user")
	if err != nil || len(requests) != 1 || requests[0].State != "waiting_air_date" {
		t.Fatalf("upcoming summary=%+v err=%v", requests, err)
	}
}
