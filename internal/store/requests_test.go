package store

import (
	"context"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func TestRequestMigrationPreservesLegacyRequesters(t *testing.T) {
	var ctx = context.Background()
	var st = openTestStore(t)
	var now = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	if err := ensureMigrationTable(ctx, st); err != nil {
		t.Fatal(err)
	}
	versions, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range versions {
		if version.Version < 6 {
			if err := applyMigration(ctx, st, version); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := st.Profiles().Seed(ctx, []model.QualityProfile{testProfile()}); err != nil {
		t.Fatal(err)
	}
	profile, err := st.Profiles().Default(ctx)
	if err != nil {
		t.Fatal(err)
	}
	movie, err := st.Movies().Create(ctx, model.Movie{Title: "Shared movie", TMDBID: 99,
		ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "legacy request")
	if err != nil {
		t.Fatal(err)
	}
	for _, userID := range []string{"alice", "bob"} {
		if err := st.Recommendations().UpsertUser(ctx, model.JellyfinUser{
			ServerID: "server", UserID: userID, DisplayName: userID, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.Recommendations().Replace(ctx, "server", userID, "movie", []model.Recommendation{{
			TMDBID: 99, Title: movie.Title, Score: 75, Reasons: []string{},
			GeneratedAt: now, ExpiresAt: now.Add(time.Hour),
		}}); err != nil {
			t.Fatal(err)
		}
		recommendations, err := st.Recommendations().List(ctx, "server", userID, "movie", "active", 10, 0)
		if err != nil || len(recommendations) != 1 {
			t.Fatalf("recommendations=%+v err=%v", recommendations, err)
		}
		if _, _, err := st.Recommendations().RecordAction(ctx, recommendations[0].ID, userID+"-request", "request"); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, st, testLogger()); err != nil {
		t.Fatal(err)
	}
	// Attribution remains after the transient recommendations are removed.
	if _, err := st.Writer().ExecContext(ctx, "DELETE FROM recommendations"); err != nil {
		t.Fatal(err)
	}
	for _, userID := range []string{"alice", "bob"} {
		requests, err := st.Requests().List(ctx, "server", userID)
		if err != nil || len(requests) != 1 {
			t.Fatalf("%s requests=%+v err=%v", userID, requests, err)
		}
		if requests[0].UserID != userID || requests[0].SubjectID != movie.ID || requests[0].Available {
			t.Fatalf("incorrect attribution or unconfirmed availability: %+v", requests[0])
		}
	}
}
