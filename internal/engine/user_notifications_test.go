package engine

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type seenRequest struct {
	Method, Path, ContentType, Body string
	Header                          http.Header
}

// recorder is an httptest receiver that records every request and answers with
// the status returned by respond (204 when nil).
type recorder struct {
	*httptest.Server
	mu      sync.Mutex
	seen    []seenRequest
	respond func(n int) int
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	var rec = &recorder{}

	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body, _ = io.ReadAll(r.Body)

		rec.mu.Lock()
		rec.seen = append(rec.seen, seenRequest{Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type"), Body: string(body), Header: r.Header.Clone()})
		var n = len(rec.seen)
		rec.mu.Unlock()
		if rec.respond != nil {
			w.WriteHeader(rec.respond(n))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

type userNotificationFixture struct {
	db  *store.Store
	clk *clock.Fake
	cfg *config.Config
	eng *Engine
}

// newUserNotificationFixture queues one available title for "user". mutate
// runs before the sync, so a destination saved there receives the event.
func newUserNotificationFixture(t *testing.T, mutate func(*store.Store, *config.Config)) *userNotificationFixture {
	t.Helper()
	var ctx = context.Background()
	var clk = clock.NewFake(time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	var db, log, profile = journeyStore(t, clk.Now)
	var cfg = testConfig()
	var movie, err = db.Movies().Create(ctx, model.Movie{Title: "Available", TMDBID: 1, ProfileID: profile.ID, RootFolder: t.TempDir(), State: model.StateWanted}, "fixture")

	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Requests().Create(ctx, model.MediaRequest{ServerID: "server", UserID: "user", MediaType: "movie", TMDBID: 1, Title: "Available", SubjectType: "movie", SubjectID: movie.ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.Recommendations().UpsertItems(ctx, []model.JellyfinItem{{ServerID: "server", ItemID: "playable id", MediaType: "movie", TMDBID: 1, Title: "Available", Present: true}}, "sync"); err != nil {
		t.Fatal(err)
	}
	cfg.Availability.JellyfinServers = map[string]string{"server": "http://jellyfin.test/jf/"}
	mutate(db, cfg)
	if _, err := db.Recommendations().CompleteSync(ctx, "server", "sync"); err != nil {
		t.Fatal(err)
	}
	eng, err := New(Options{Store: db, Config: cfg, Downloader: &fakeDownloader{}, Clock: clk, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	return &userNotificationFixture{db: db, clk: clk, cfg: cfg, eng: eng}
}

func saveDestination(t *testing.T, db *store.Store, value store.NotificationDestination) {
	t.Helper()
	value.ServerID, value.UserID = "server", "user"
	if err := db.NotificationDestinations().Put(context.Background(), value); err != nil {
		t.Fatal(err)
	}
}

func (f *userNotificationFixture) run(t *testing.T) {
	t.Helper()
	if err := f.eng.NotificationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *userNotificationFixture) last(t *testing.T) store.UserDelivery {
	t.Helper()
	var delivery, err = f.db.NotificationDestinations().LastUserDelivery(context.Background(), "server", "user")

	if err != nil {
		t.Fatal(err)
	}
	return delivery
}

func TestUserNtfyDeliveryUsesTopicHeadersAndClickLink(t *testing.T) {
	var ntfy = newRecorder(t)
	var f = newUserNotificationFixture(t, func(db *store.Store, cfg *config.Config) {
		cfg.Availability.UserNtfyBaseURL = ntfy.URL + "/base"
		saveDestination(t, db, store.NotificationDestination{Kind: "ntfy", Topic: "my-topic_1", Format: "json", Enabled: true})
	})

	f.run(t)
	if ntfy.count() != 1 {
		t.Fatalf("ntfy requests = %d, want 1", ntfy.count())
	}
	var got = ntfy.seen[0]

	if got.Method != http.MethodPost || got.Path != "/base/my-topic_1" {
		t.Errorf("request = %s %s", got.Method, got.Path)
	}
	if got.Body != "Available is available in Jellyfin." || got.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("body %q content type %q", got.Body, got.ContentType)
	}
	if got.Header.Get("Title") != "Reelay: ready to watch" || got.Header.Get("Tags") != "popcorn" {
		t.Errorf("headers = %v", got.Header)
	}
	if want := "http://jellyfin.test/jf/web/index.html#!/details?id=playable+id"; got.Header.Get("Click") != want {
		t.Errorf("Click = %q, want %q", got.Header.Get("Click"), want)
	}
	if got.Header.Get("X-Reelay-Event-ID") == "" {
		t.Error("missing X-Reelay-Event-ID")
	}
	if last := f.last(t); last.DeliveredAt == nil || last.LastError != "" {
		t.Errorf("delivery = %+v", last)
	}
	f.run(t)
	if ntfy.count() != 1 {
		t.Errorf("delivered event was sent again: %d requests", ntfy.count())
	}
}

func TestUserWebhookDeliveryFormats(t *testing.T) {
	for _, tc := range []struct{ format, contentType, bodyPart string }{
		{"json", "application/json", `"event":"title_available"`},
		{"ntfy", "text/plain; charset=utf-8", "Available is available in Jellyfin."},
	} {
		var hook = newRecorder(t)

		newUserNotificationFixture(t, func(db *store.Store, cfg *config.Config) {
			saveDestination(t, db, store.NotificationDestination{Kind: "webhook", URL: hook.URL + "/hook", Format: tc.format, Enabled: true})
		}).run(t)
		if hook.count() != 1 {
			t.Fatalf("%s: requests = %d, want 1", tc.format, hook.count())
		}
		var got = hook.seen[0]

		if got.Path != "/hook" || got.ContentType != tc.contentType || !strings.Contains(got.Body, tc.bodyPart) {
			t.Errorf("%s: %s %q %q", tc.format, got.Path, got.ContentType, got.Body)
		}
		if tc.format == "json" && got.Header.Get("Title") != "" {
			t.Errorf("json webhook got ntfy headers: %v", got.Header)
		}
	}
}

func TestUserNtfyWithoutBaseURLFailsWithLongBackoff(t *testing.T) {
	var f = newUserNotificationFixture(t, func(db *store.Store, cfg *config.Config) {
		saveDestination(t, db, store.NotificationDestination{Kind: "ntfy", Topic: "topic", Format: "json", Enabled: true})
	})

	f.run(t)
	if last := f.last(t); last.DeliveredAt != nil || last.LastError != "per-user ntfy is not configured" {
		t.Fatalf("delivery = %+v", last)
	}
	f.clk.Advance(time.Hour)
	f.run(t)
	if last := f.last(t); last.Attempts != 1 {
		t.Errorf("attempts after 1h = %d, want 1 (long backoff)", last.Attempts)
	}
	f.clk.Advance(6 * time.Hour)
	f.run(t)
	if last := f.last(t); last.Attempts != 2 {
		t.Errorf("attempts after backoff = %d, want 2", last.Attempts)
	}
}

func TestUserNotificationForRemovedDestinationIsClosed(t *testing.T) {
	for name, remove := range map[string]func(*testing.T, *store.Store){
		"deleted": func(t *testing.T, db *store.Store) {
			if err := db.NotificationDestinations().Delete(context.Background(), "server", "user"); err != nil {
				t.Fatal(err)
			}
		},
		"disabled": func(t *testing.T, db *store.Store) {
			saveDestination(t, db, store.NotificationDestination{Kind: "webhook", URL: "http://127.0.0.1:1/never", Format: "json", Enabled: false})
		},
	} {
		var hook = newRecorder(t)
		var f = newUserNotificationFixture(t, func(db *store.Store, cfg *config.Config) {
			saveDestination(t, db, store.NotificationDestination{Kind: "webhook", URL: hook.URL, Format: "json", Enabled: true})
		})

		remove(t, f.db)
		f.run(t)
		if last := f.last(t); last.DeliveredAt == nil || last.LastError != "destination removed" {
			t.Errorf("%s: delivery = %+v", name, last)
		}
		if hook.count() != 0 {
			t.Errorf("%s: removed destination still received %d requests", name, hook.count())
		}
	}
}

func TestUserNotificationRetriesWithBackoff(t *testing.T) {
	var hook = newRecorder(t)
	var f = newUserNotificationFixture(t, func(db *store.Store, cfg *config.Config) {
		saveDestination(t, db, store.NotificationDestination{Kind: "webhook", URL: hook.URL, Format: "json", Enabled: true})
	})

	hook.respond = func(n int) int {
		if n == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	}
	f.run(t)
	f.run(t)
	if hook.count() != 1 {
		t.Fatalf("retried before due: %d requests", hook.count())
	}
	if last := f.last(t); last.DeliveredAt != nil || last.LastError != "webhook returned HTTP 500" {
		t.Fatalf("delivery = %+v", last)
	}
	f.clk.Advance(time.Minute)
	f.run(t)
	if hook.count() != 2 {
		t.Fatalf("requests after backoff = %d, want 2", hook.count())
	}
	if hook.seen[0].Header.Get("X-Reelay-Event-ID") != hook.seen[1].Header.Get("X-Reelay-Event-ID") {
		t.Error("event ID changed between attempts")
	}
	if last := f.last(t); last.DeliveredAt == nil || last.LastError != "" {
		t.Errorf("delivery = %+v", last)
	}
}

func TestGlobalAndUserOutboxesAreDeliveredIndependently(t *testing.T) {
	var global = newRecorder(t)
	var user = newRecorder(t)
	var f = newUserNotificationFixture(t, func(db *store.Store, cfg *config.Config) {
		cfg.Availability.WebhookURL = global.URL
		saveDestination(t, db, store.NotificationDestination{Kind: "webhook", URL: user.URL, Format: "json", Enabled: true})
	})

	global.respond = func(int) int { return http.StatusInternalServerError }
	f.run(t)
	if global.count() != 1 || user.count() != 1 {
		t.Fatalf("global %d, user %d requests, want 1 each", global.count(), user.count())
	}
	if last := f.last(t); last.DeliveredAt == nil {
		t.Errorf("user delivery blocked by failing global webhook: %+v", last)
	}
	global.respond = nil
	f.clk.Advance(time.Minute)
	f.run(t)
	if global.count() != 2 || user.count() != 1 {
		t.Errorf("global %d, user %d requests, want 2 and 1", global.count(), user.count())
	}
	if _, err := f.db.Requests().ClaimNotification(context.Background()); err == nil {
		t.Error("global event still pending")
	}
}

func TestUserOutboxIsDeliveredWithoutGlobalWebhook(t *testing.T) {
	var user = newRecorder(t)
	var f = newUserNotificationFixture(t, func(db *store.Store, cfg *config.Config) {
		saveDestination(t, db, store.NotificationDestination{Kind: "webhook", URL: user.URL, Format: "json", Enabled: true})
	})

	if f.cfg.Availability.WebhookURL != "" {
		t.Fatal("fixture has a global webhook")
	}
	f.run(t)
	if user.count() != 1 {
		t.Fatalf("user requests = %d, want 1", user.count())
	}
}
