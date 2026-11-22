package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func importedMovie(t *testing.T, s *Store, profileID int64, title string, state model.ItemState, path string) model.Movie {
	t.Helper()
	movie, err := s.Movies().Create(context.Background(), model.Movie{Title: title, Year: 2020, ProfileID: profileID,
		RootFolder: "/movies", State: state, ImportedPath: path, ImportedQuality: "720p webdl"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return movie
}

func TestInflightUpgradesCountOnlyItemsWithAnImportedFile(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	importedMovie(t, s, p.ID, "Settled", model.StateImported, "/movies/a.mkv")
	importedMovie(t, s, p.ID, "Fresh want", model.StateWanted, "")
	importedMovie(t, s, p.ID, "Failed", model.StateFailed, "/movies/f.mkv")
	for i, state := range []model.ItemState{model.StateWanted, model.StateSearching, model.StateDownloading} {
		importedMovie(t, s, p.ID, "Upgrading "+string(state), state, fmt.Sprintf("/movies/u%d.mkv", i))
	}
	series, err := s.Series().Create(ctx, model.Series{Title: "Show", ProfileID: p.ID, RootFolder: "/tv", MonitorMode: model.MonitorAll})
	if err != nil {
		t.Fatal(err)
	}
	for number, state := range map[int]model.ItemState{1: model.StateImported, 2: model.StateImporting, 3: model.StateGrabbed} {
		if _, err := s.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1, Number: number,
			State: state, ImportedPath: "/tv/e.mkv", ImportedQuality: "720p webdl"}, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Movies().InflightUpgrades(ctx); err != nil || n != 3 {
		t.Errorf("movie in-flight upgrades = %d, %v; want 3", n, err)
	}
	if n, err := s.Episodes().InflightUpgrades(ctx); err != nil || n != 2 {
		t.Errorf("episode in-flight upgrades = %d, %v; want 2", n, err)
	}
}

func TestUpgradeDueOrdersNeverCheckedFirstThenOldest(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	now := time.Now().UTC()
	oneDay := importedMovie(t, s, p.ID, "one day", model.StateImported, "/m/1.mkv")
	never := importedMovie(t, s, p.ID, "never", model.StateImported, "/m/2.mkv")
	twoDays := importedMovie(t, s, p.ID, "two days", model.StateImported, "/m/3.mkv")
	recent := importedMovie(t, s, p.ID, "recent", model.StateImported, "/m/4.mkv")
	importedMovie(t, s, p.ID, "not imported", model.StateWanted, "/m/5.mkv")
	for id, at := range map[int64]time.Time{oneDay.ID: now.Add(-24 * time.Hour), twoDays.ID: now.Add(-48 * time.Hour), recent.ID: now.Add(-time.Minute)} {
		if err := s.Movies().MarkUpgradeChecked(ctx, []int64{id}, at); err != nil {
			t.Fatal(err)
		}
	}
	due, err := s.Movies().UpgradeDue(ctx, now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for _, m := range due {
		got = append(got, m.ID)
	}
	want := []int64{never.ID, twoDays.ID, oneDay.ID}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("due order = %v, want %v", got, want)
	}
	if due[0].UpgradeCheckedAt != nil || due[1].UpgradeCheckedAt == nil {
		t.Errorf("upgrade_checked_at not scanned: %v / %v", due[0].UpgradeCheckedAt, due[1].UpgradeCheckedAt)
	}
	if limited, err := s.Movies().UpgradeDue(ctx, now.Add(-time.Hour), 2); err != nil || len(limited) != 2 {
		t.Errorf("limit 2 returned %d, %v", len(limited), err)
	}
}

func TestEpisodeUpgradeDueAndMarkChecked(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	series, err := s.Series().Create(ctx, model.Series{Title: "Show", ProfileID: p.ID, RootFolder: "/tv", MonitorMode: model.MonitorAll})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for number := 1; number <= 2; number++ {
		ep, err := s.Episodes().Create(ctx, model.Episode{SeriesID: series.ID, Season: 1, Number: number,
			State: model.StateImported, ImportedPath: "/tv/e.mkv", ImportedQuality: "720p webdl"}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ep.ID)
	}
	now := time.Now().UTC()
	if err := s.Episodes().MarkUpgradeChecked(ctx, ids[:1], now); err != nil {
		t.Fatal(err)
	}
	due, err := s.Episodes().UpgradeDue(ctx, now.Add(-time.Hour), 10)
	if err != nil || len(due) != 1 || due[0].ID != ids[1] {
		t.Fatalf("due = %+v, %v; want only the unchecked episode", due, err)
	}
	checked, err := s.Episodes().Get(ctx, ids[0])
	if err != nil || checked.UpgradeCheckedAt == nil || checked.UpgradeCheckedAt.Sub(now).Abs() > time.Second {
		t.Fatalf("checked episode = %+v, %v", checked.UpgradeCheckedAt, err)
	}
	if err := s.Episodes().MarkUpgradeChecked(ctx, nil, now); err != nil {
		t.Errorf("empty id list: %v", err)
	}
}

func TestStartUpgradeReturnsAnImportedItemToWantedAndKeepsTheFile(t *testing.T) {
	ctx := context.Background()
	s := migratedDomainStore(t)
	p, _ := s.Profiles().Default(ctx)
	movie := importedMovie(t, s, p.ID, "Primer", model.StateImported, "/m/primer.mkv")
	if err := s.Transitions().StartUpgrade(ctx, model.SubjectMovie, movie.ID, "upgrade_search", "imported 720p webdl below cutoff 1080p"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Movies().Get(ctx, movie.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StateWanted || got.ImportedPath != "/m/primer.mkv" || got.NextSearchAt != nil ||
		got.SearchAttempts != 0 || got.UpgradeCheckedAt == nil {
		t.Fatalf("after start: %+v", got)
	}
	history, err := s.Transitions().History(ctx, model.SubjectMovie, movie.ID, 1)
	if err != nil || len(history) != 1 || history[0].Reason != "upgrade_search" || history[0].From != model.StateImported {
		t.Fatalf("history = %+v, %v", history, err)
	}
	if err := s.Transitions().StartUpgrade(ctx, model.SubjectMovie, movie.ID, "upgrade_search", ""); !errors.Is(err, ErrItemBusy) {
		t.Errorf("second start = %v, want ErrItemBusy", err)
	}
	if err := s.Transitions().StartUpgrade(ctx, model.SubjectMovie, 9999, "upgrade_search", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown item = %v, want ErrNotFound", err)
	}
}
