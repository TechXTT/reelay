package store

import (
	"context"
	"errors"
	"testing"

	"github.com/TechXTT/reelay/internal/model"
)

// destinationFixture has users "alice" and "bob", each with one movie request
// for a distinct title that is present in Jellyfin. It returns request IDs by user.
func destinationFixture(t *testing.T) (*Store, map[string]int64) {
	t.Helper()
	var ctx = context.Background()
	var s = migratedDomainStore(t)
	var profile, err = s.Profiles().Default(ctx)
	var ids = map[string]int64{}
	var items []model.JellyfinItem

	if err != nil {
		t.Fatal(err)
	}
	for i, user := range []string{"alice", "bob"} {
		var tmdb = i + 1

		if err := s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "srv", UserID: user, DisplayName: user, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		movie, err := s.Movies().Create(ctx, model.Movie{Title: "Movie " + user, TMDBID: tmdb, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Requests().Create(ctx, model.MediaRequest{ServerID: "srv", UserID: user, MediaType: "movie", TMDBID: tmdb, Title: "Movie " + user, SubjectType: "movie", SubjectID: movie.ID}); err != nil {
			t.Fatal(err)
		}
		var id int64

		if err := s.ro.QueryRow(`SELECT id FROM media_requests WHERE user_id=?`, user).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[user] = id
		items = append(items, model.JellyfinItem{ServerID: "srv", ItemID: "item-" + user, MediaType: "movie", TMDBID: tmdb, Title: "Movie " + user, Present: true})
	}
	if err := s.Recommendations().UpsertItems(ctx, items, "sync"); err != nil {
		t.Fatal(err)
	}
	return s, ids
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int

	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNotificationDestinationCRUD(t *testing.T) {
	var ctx = context.Background()
	var s, _ = destinationFixture(t)
	var repo = s.NotificationDestinations()

	if _, err := repo.Get(ctx, "srv", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before Put = %v, want ErrNotFound", err)
	}
	if err := repo.Put(ctx, NotificationDestination{ServerID: "srv", UserID: "alice", Kind: "ntfy", Topic: "alice-topic", Format: "json", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	first, err := repo.Get(ctx, "srv", "alice")
	if err != nil || first.Topic != "alice-topic" || !first.Enabled || first.Kind != "ntfy" {
		t.Fatalf("Get = %+v, %v", first, err)
	}
	if err := repo.Put(ctx, NotificationDestination{ServerID: "srv", UserID: "alice", Kind: "webhook", URL: "https://example.test/hook", Format: "ntfy", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	second, err := repo.Get(ctx, "srv", "alice")
	if err != nil || second.Kind != "webhook" || second.URL != "https://example.test/hook" || second.Format != "ntfy" || second.Enabled || second.Topic != "" {
		t.Fatalf("after upsert = %+v, %v", second, err)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at changed on upsert: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	if err := repo.Put(ctx, NotificationDestination{ServerID: "srv", UserID: "ghost", Kind: "ntfy", Topic: "x", Format: "json", Enabled: true}); err == nil {
		t.Error("Put for an unknown Jellyfin user succeeded")
	}
	if err := repo.Delete(ctx, "srv", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, "srv", "alice"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestAvailabilityQueuesUserEventsOnlyForEnabledDestinations(t *testing.T) {
	var ctx = context.Background()
	var s, _ = destinationFixture(t)
	var repo = s.NotificationDestinations()

	if err := repo.Put(ctx, NotificationDestination{ServerID: "srv", UserID: "alice", Kind: "ntfy", Topic: "alice", Format: "json", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Put(ctx, NotificationDestination{ServerID: "srv", UserID: "bob", Kind: "ntfy", Topic: "bob", Format: "json", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Recommendations().CompleteSync(ctx, "srv", "sync"); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, s, "user_availability_outbox"); got != 1 {
		t.Fatalf("user outbox rows = %d, want 1 (enabled destination only, idempotent)", got)
	}
	if got := countRows(t, s, "availability_outbox"); got != 2 {
		t.Fatalf("global outbox rows = %d, want 2 regardless of destinations", got)
	}
	claimed, err := s.Requests().ClaimUserNotification(ctx)
	if err != nil || !claimed.HasDestination || claimed.Destination.Topic != "alice" {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	pending, err := s.Requests().PendingUserNotifications(ctx)
	if err != nil || pending != 1 {
		t.Fatalf("pending = %d, %v", pending, err)
	}
}

func TestDestinationSavedAfterAvailabilityDoesNotReplayTitles(t *testing.T) {
	var ctx = context.Background()
	var s, _ = destinationFixture(t)

	if _, err := s.Recommendations().CompleteSync(ctx, "srv", "sync"); err != nil {
		t.Fatal(err)
	}
	if err := s.NotificationDestinations().Put(ctx, NotificationDestination{ServerID: "srv", UserID: "alice", Kind: "ntfy", Topic: "alice", Format: "json", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recommendations().CompleteSync(ctx, "srv", "sync"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, "user_availability_outbox"); got != 0 {
		t.Fatalf("user outbox rows = %d, want 0 for titles already announced", got)
	}
}

func TestCancelDeletesUndeliveredUserEvents(t *testing.T) {
	var ctx = context.Background()
	var s, ids = destinationFixture(t)

	for _, user := range []string{"alice", "bob"} {
		if err := s.NotificationDestinations().Put(ctx, NotificationDestination{ServerID: "srv", UserID: user, Kind: "ntfy", Topic: user, Format: "json", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Recommendations().CompleteSync(ctx, "srv", "sync"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, s, "user_availability_outbox"); got != 2 {
		t.Fatalf("user outbox rows = %d, want 2", got)
	}
	// Alice's event is delivered; both users then withdraw. Only the delivered
	// row survives, because withdrawal removes undelivered events only.
	claimed, err := s.Requests().ClaimUserNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Requests().FinishUserNotification(ctx, claimed.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := s.Requests().Cancel(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, s, "user_availability_outbox"); got != 1 {
		t.Fatalf("user outbox rows = %d, want only the delivered one kept", got)
	}
	if pending, _ := s.Requests().PendingUserNotifications(ctx); pending != 0 {
		t.Errorf("pending = %d, want 0", pending)
	}
}

func TestDropUserNotificationClosesTheEvent(t *testing.T) {
	var ctx = context.Background()
	var s, _ = destinationFixture(t)

	if err := s.NotificationDestinations().Put(ctx, NotificationDestination{ServerID: "srv", UserID: "alice", Kind: "ntfy", Topic: "alice", Format: "json", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recommendations().CompleteSync(ctx, "srv", "sync"); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Requests().ClaimUserNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.NotificationDestinations().Delete(ctx, "srv", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.Requests().DropUserNotification(ctx, claimed.ID, "destination removed"); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.Requests().PendingUserNotifications(ctx); pending != 0 {
		t.Errorf("pending = %d, want 0", pending)
	}
	last, err := s.NotificationDestinations().LastUserDelivery(ctx, "srv", "alice")
	if err != nil || last.LastError != "destination removed" || last.DeliveredAt == nil {
		t.Errorf("LastUserDelivery = %+v, %v", last, err)
	}
}
