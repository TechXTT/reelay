package store

import (
	"context"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func TestPreferencesIdentityValidationAndDismissalUndo(t *testing.T) {
	var ctx = context.Background()
	var st = migratedDomainStore(t)
	var preferences = RecommendationPreferences{Languages: []string{"EN", "ja"}, ExcludedGenres: []string{" Horror "}, Familiarity: "explore", Diversity: 25}

	if err := st.Recommendations().SavePreferences(ctx, "server", "alice", preferences); err != nil {
		t.Fatal(err)
	}
	saved, err := st.Recommendations().Preferences(ctx, "server", "alice")
	if err != nil || saved.Languages[0] != "en" || saved.ExcludedGenres[0] != "Horror" || saved.Diversity != 25 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	other, err := st.Recommendations().Preferences(ctx, "server", "bob")
	if err != nil || other.Diversity != 100 || len(other.Languages) != 0 {
		t.Fatalf("leaked preferences: %+v %v", other, err)
	}
	preferences.Languages = []string{"English"}
	if err := st.Recommendations().SavePreferences(ctx, "server", "alice", preferences); err == nil {
		t.Fatal("invalid language accepted")
	}
	if err := st.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "server", UserID: "alice", DisplayName: "Alice", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().Replace(ctx, "server", "alice", "movie", []model.Recommendation{{TMDBID: 77, Title: "Dismissed", GeneratedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	values, err := st.Recommendations().List(ctx, "server", "alice", "movie", "", 10, 0)
	if err != nil || len(values) != 1 {
		t.Fatalf("recommendations: %v %v", values, err)
	}
	if _, _, err := st.Recommendations().RecordAction(ctx, values[0].ID, "dismiss", "dismiss"); err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().UndoDismissal(ctx, values[0].ID); err != nil {
		t.Fatal(err)
	}
	excluded, err := st.Recommendations().ExcludedTMDBIDs(ctx, "server", "alice", "movie")
	if err != nil || excluded[77] {
		t.Fatalf("undo retained exclusion: %v %v", excluded, err)
	}
	if _, err := st.Recommendations().RecordRating(ctx, values[0].ID, "rate", 2); err != nil {
		t.Fatal(err)
	}
	if err := st.Recommendations().UndoDismissal(ctx, values[0].ID); err == nil {
		t.Fatal("undo erased rating semantics")
	}
}
