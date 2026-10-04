package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func TestAvailabilityOutboxAndWithdrawalPreserveSharedSubject(t *testing.T) {
	var ctx = context.Background()
	var st = migratedDomainStore(t)
	var profile, err = st.Profiles().Default(ctx)

	if err != nil {
		t.Fatal(err)
	}
	movie, err := st.Movies().Create(ctx, model.Movie{Title: "Shared", TMDBID: 99, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob"} {
		if err := st.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: user, DisplayName: user, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: user, MediaType: "movie", TMDBID: 99, Title: "Shared", SubjectType: "movie", SubjectID: movie.ID}); err != nil {
			t.Fatal(err)
		}
	}
	requests, err := st.Requests().List(ctx, "server", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Requests().Cancel(ctx, requests[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().UpsertItems(ctx, []model.JellyfinItem{{ServerID: "server", ItemID: "playable", Title: "Shared", MediaType: "movie", TMDBID: 99, Present: true}}, "sync"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := st.Recommendations().CompleteSync(ctx, "server", "sync"); err != nil {
			t.Fatal(err)
		}
	}
	notification, err := st.Requests().ClaimNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Requests().ClaimNotification(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate claim: %v", err)
	}
	if err := st.Requests().FinishNotification(ctx, notification.ID, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recommendations().CompleteSync(ctx, "server", "sync"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Requests().ClaimNotification(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("duplicate event: %v", err)
	}
	stored, err := st.Movies().Get(ctx, movie.ID)
	if err != nil || stored.State != model.StateWanted {
		t.Fatalf("withdrawal changed shared movie: %+v %v", stored, err)
	}
	requests, err = st.Requests().List(ctx, "server", "alice")
	if err != nil || requests[0].State != "cancelled" {
		t.Fatalf("withdrawn request: %+v %v", requests, err)
	}
	requests, err = st.Requests().List(ctx, "server", "bob")
	if err != nil || !requests[0].Available || requests[0].JellyfinItemID != "playable" {
		t.Fatalf("other requester: %+v %v", requests, err)
	}
}

func TestSelectedSeasonsAccumulateWithoutNarrowing(t *testing.T) {
	var ctx = context.Background()
	var st = migratedDomainStore(t)
	var profile, err = st.Profiles().Default(ctx)

	if err != nil {
		t.Fatal(err)
	}
	series, err := st.Series().Create(ctx, model.Series{Title: "Seasons", TMDBID: 98, ProfileID: profile.ID, RootFolder: t.TempDir(), MonitorMode: model.MonitorAll})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: "alice", DisplayName: "Alice", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "alice", MediaType: "series", TMDBID: 98, Title: series.Title, SubjectType: "series", SubjectID: series.ID}); err != nil {
		t.Fatal(err)
	}
	for _, seasons := range [][]int{{2, 1, 2}, {0, 3}} {
		if err := st.Requests().AddSeasons(ctx, "server", "alice", 98, series.ID, seasons); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := st.Requests().SelectedSeasons(ctx, series.ID)
	if err != nil || len(selected) != 4 || selected[0] != 0 || selected[3] != 3 {
		t.Fatalf("selected seasons: %v %v", selected, err)
	}
	stored, err := st.Series().Get(ctx, series.ID)
	if err != nil || stored.MonitorMode != model.MonitorAll {
		t.Fatalf("narrowed monitoring: %+v %v", stored, err)
	}
}

func TestSelectedSeasonSummaryExcludesOtherUsersFailuresAndCanWiden(t *testing.T) {
	ctx := context.Background()
	st := migratedDomainStore(t)
	profile, err := st.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	series, err := st.Series().Create(ctx, model.Series{Title: "Scoped", TMDBID: 77, ProfileID: profile.ID, RootFolder: t.TempDir(), MonitorMode: model.MonitorAll, Status: model.SeriesFollowing})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: "user", DisplayName: "User", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	request := model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "series", TMDBID: 77, Title: "Scoped", SubjectType: "series", SubjectID: series.ID}
	if _, err := st.Requests().Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := st.Requests().AddSeasons(ctx, "server", "user", 77, series.ID, []int{1}); err != nil {
		t.Fatal(err)
	}
	for season, state := range map[int]model.ItemState{1: model.StateImported, 2: model.StateFailed} {
		if _, _, err := st.Episodes().UpsertMetadata(ctx, model.Episode{SeriesID: series.ID, Season: season, Number: 1}, state, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	values, err := st.Requests().List(ctx, "server", "user")
	if err != nil || len(values) != 1 || values[0].State != "imported" || values[0].TotalEpisodes != 1 {
		t.Fatalf("unrelated failure affected selected scope: %+v %v", values, err)
	}
	attention, err := st.Requests().ListPage(ctx, "server", "user", 0, true)
	if err != nil || len(attention) != 0 {
		t.Fatalf("unrelated failure needs attention: %+v %v", attention, err)
	}
	request.MonitorMode = "all"
	if _, err := st.Requests().Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	attention, err = st.Requests().ListPage(ctx, "server", "user", 0, true)
	if err != nil || len(attention) != 1 || attention[0].TotalEpisodes != 2 {
		t.Fatalf("widened scope omitted failure: %+v %v", attention, err)
	}
}
