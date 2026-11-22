package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/model"
)

func offer(t *testing.T, s *Store, userID, mediaType string, at time.Time, scores map[int]float64) {
	t.Helper()
	var values []model.Recommendation

	for id, score := range scores {
		values = append(values, model.Recommendation{TMDBID: id, Title: "Title", Score: score, Reasons: []string{"reason"}, GeneratedAt: at, ExpiresAt: at.Add(time.Hour)})
	}
	if err := s.Recommendations().ReplaceOffered(context.Background(), "s", userID, mediaType, values); err != nil {
		t.Fatal(err)
	}
}

func exec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.rw.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestImpressionsKeepFirstOfferAndBestRank(t *testing.T) {
	var s = migratedDomainStore(t)
	var ctx = context.Background()
	var t1 = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	var t2 = t1.Add(24 * time.Hour)
	var t3 = t2.Add(24 * time.Hour)

	type row struct {
		first, last string
		rank        int
		score       float64
	}
	var want = map[int]row{
		10: {FormatTime(t1), FormatTime(t3), 1, 50},
		20: {FormatTime(t1), FormatTime(t3), 1, 60},
		30: {FormatTime(t3), FormatTime(t3), 3, 1},
	}

	_ = s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "s", UserID: "u", DisplayName: "U", Enabled: true})
	offer(t, s, "u", "movie", t1, map[int]float64{10: 50, 20: 60})
	offer(t, s, "u", "movie", t2, map[int]float64{10: 90, 20: 40})
	offer(t, s, "u", "movie", t3, map[int]float64{10: 10, 20: 5, 30: 1})

	for id, w := range want {
		var got row

		err := s.ro.QueryRowContext(ctx, `SELECT first_offered_at,last_offered_at,best_rank,first_score FROM recommendation_impressions WHERE server_id='s' AND user_id='u' AND media_type='movie' AND tmdb_id=?`, id).Scan(&got.first, &got.last, &got.rank, &got.score)
		if err != nil || got != w {
			t.Errorf("impression %d = %+v err=%v, want %+v", id, got, err, w)
		}
	}
}

func TestImpressionsAreLimitedToMaterializedTitles(t *testing.T) {
	var s = migratedDomainStore(t)
	var scores = map[int]float64{}
	var recorded, lowest int

	_ = s.Recommendations().UpsertUser(context.Background(), model.JellyfinUser{ServerID: "s", UserID: "u", DisplayName: "U", Enabled: true})
	for id := 1; id <= MaterializedRecommendationLimit+5; id++ {
		scores[id] = float64(id)
	}
	offer(t, s, "u", "movie", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), scores)
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM recommendation_impressions`).Scan(&recorded); err != nil || recorded != MaterializedRecommendationLimit {
		t.Fatalf("impressions = %d err=%v, want %d", recorded, err, MaterializedRecommendationLimit)
	}
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM recommendation_impressions WHERE tmdb_id<=5`).Scan(&lowest); err != nil || lowest != 0 {
		t.Fatalf("lowest-scoring titles recorded = %d err=%v, want 0", lowest, err)
	}
}

// The harness migrates stepwise: it applies 0001-0009, seeds recommendations,
// then lets Migrate apply 0010 so the backfill runs against real rows.
func TestImpressionBackfillMigration(t *testing.T) {
	var s = openTestStore(t)
	var ctx = context.Background()
	var at = FormatTime(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC))
	var movies, best, worst, seriesRank, topID, lowest int
	var first, last string
	var score float64

	var ms, err = loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMigrationTable(ctx, s); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Version < 10 {
			if err := applyMigration(ctx, s, m); err != nil {
				t.Fatal(err)
			}
		}
	}
	exec(t, s, `INSERT INTO jellyfin_users(server_id,user_id,display_name,created_at,updated_at) VALUES('s','u','U',?,?)`, at, at)
	for id := 1; id <= MaterializedRecommendationLimit+2; id++ {
		exec(t, s, `INSERT INTO recommendations(server_id,user_id,media_type,tmdb_id,title,score,reasons_json,features_json,generated_at,expires_at) VALUES('s','u','movie',?,'T',?,'[]','{}',?,?)`, id, float64(id), at, at)
	}
	exec(t, s, `INSERT INTO recommendations(server_id,user_id,media_type,tmdb_id,title,score,reasons_json,features_json,generated_at,expires_at) VALUES('s','u','series',900,'S',7,'[]','{}',?,?)`, at, at)

	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}

	if err := s.ro.QueryRow(`SELECT COUNT(*),MIN(best_rank),MAX(best_rank) FROM recommendation_impressions WHERE media_type='movie'`).Scan(&movies, &best, &worst); err != nil {
		t.Fatal(err)
	}
	if movies != MaterializedRecommendationLimit || best != 1 || worst != MaterializedRecommendationLimit {
		t.Errorf("movie impressions=%d ranks %d..%d, want %d ranks 1..%d", movies, best, worst, MaterializedRecommendationLimit, MaterializedRecommendationLimit)
	}
	if err := s.ro.QueryRow(`SELECT tmdb_id,first_offered_at,last_offered_at,first_score FROM recommendation_impressions WHERE media_type='movie' AND best_rank=1`).Scan(&topID, &first, &last, &score); err != nil {
		t.Fatal(err)
	}
	if topID != MaterializedRecommendationLimit+2 || first != at || last != at || score != float64(topID) {
		t.Errorf("rank 1 = tmdb %d %s %s %v", topID, first, last, score)
	}
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM recommendation_impressions WHERE media_type='movie' AND tmdb_id<=2`).Scan(&lowest); err != nil || lowest != 0 {
		t.Errorf("lowest-scoring movies backfilled = %d err=%v, want 0", lowest, err)
	}
	if err := s.ro.QueryRow(`SELECT best_rank FROM recommendation_impressions WHERE media_type='series'`).Scan(&seriesRank); err != nil || seriesRank != 1 {
		t.Errorf("series rank = %d err=%v, want 1 (ranks are per media type)", seriesRank, err)
	}
}

func TestRecommendationStatsFunnelPerUserAndWindow(t *testing.T) {
	var s = migratedDomainStore(t)
	var ctx = context.Background()
	var since = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var rows = 0
	var day = func(d int) time.Time { return time.Date(2026, 9, d, 0, 0, 0, 0, time.UTC) }
	var feedback = func(userID, action, mediaType string, id int, at time.Time) {
		rows++
		exec(t, s, `INSERT INTO recommendation_feedback(action_id,server_id,user_id,media_type,tmdb_id,action,created_at) VALUES(?,?,?,?,?,?,?)`, fmt.Sprint("a", rows), "s", userID, mediaType, id, action, FormatTime(at))
	}
	var watch = func(userID, itemID, kind string, at time.Time) {
		rows++
		exec(t, s, `INSERT INTO jellyfin_activity(event_id,server_id,user_id,item_id,event_type,occurred_at) VALUES(?,?,?,?,?,?)`, fmt.Sprint("e", rows), "s", userID, itemID, kind, FormatTime(at))
	}

	for _, id := range []string{"u1", "u2"} {
		_ = s.Recommendations().UpsertUser(ctx, model.JellyfinUser{ServerID: "s", UserID: id, DisplayName: id, Enabled: true})
	}
	// Movie 4 was first offered before the window and is only re-offered inside it.
	offer(t, s, "u1", "movie", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), map[int]float64{4: 50})
	offer(t, s, "u1", "movie", day(5), map[int]float64{1: 90, 2: 80, 3: 70, 4: 60})
	offer(t, s, "u1", "series", day(6), map[int]float64{5: 80})
	offer(t, s, "u2", "movie", day(5), map[int]float64{1: 90})
	if err := s.Recommendations().UpsertItems(ctx, []model.JellyfinItem{
		{ServerID: "s", ItemID: "i1", MediaType: "movie", TMDBID: 1, Title: "One", Present: true},
		{ServerID: "s", ItemID: "i4", MediaType: "movie", TMDBID: 4, Title: "Four", Present: true},
		{ServerID: "s", ItemID: "i5", MediaType: "series", TMDBID: 5, Title: "Five", Present: true},
	}, "sync"); err != nil {
		t.Fatal(err)
	}

	feedback("u1", "request", "movie", 1, day(6))
	feedback("u1", "request", "series", 5, day(7))
	feedback("u1", "request", "movie", 3, day(4)) // before it was offered: not an outcome
	feedback("u1", "request", "movie", 4, day(6)) // offered before the window
	feedback("u1", "dismiss", "movie", 2, day(6))
	feedback("u2", "request", "movie", 1, day(6))
	exec(t, s, `INSERT INTO recommendation_ratings(action_id,server_id,user_id,media_type,tmdb_id,rating,created_at,updated_at) VALUES('r1','s','u1','movie',1,5,?,?),('r2','s','u1','movie',2,3,?,?)`, FormatTime(day(7)), FormatTime(day(7)), FormatTime(day(7)), FormatTime(day(7)))
	watch("u1", "i1", "completed", day(8))
	watch("u1", "i5", "played", day(9))
	watch("u2", "i1", "played", day(3)) // before u2 requested it

	var want = map[string]model.RecommendationStats{
		"u1": {Offered: 4, Requested: 2, RequestRate: 0.5, Dismissed: 1, Rated: 2, AverageRating: 4, Available: 2, Watched: 2, Completed: 1},
		"u2": {Offered: 1, Requested: 1, RequestRate: 1, Available: 1},
		"":   {Offered: 5, Requested: 3, RequestRate: 0.6, Dismissed: 1, Rated: 2, AverageRating: 4, Available: 3, Watched: 2, Completed: 1},
	}

	for userID, w := range want {
		var got, err = s.Recommendations().RecommendationStats(ctx, "s", userID, since)

		if err != nil {
			t.Fatal(err)
		}
		if got.MeasuringSince == nil {
			t.Fatalf("user %q: measuring since is nil", userID)
		}
		if userID == "u1" && !got.MeasuringSince.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("measuring since = %v, want the first offer overall", got.MeasuringSince)
		}
		w.MeasuringSince = got.MeasuringSince
		if got != w {
			t.Errorf("user %q = %+v, want %+v", userID, got, w)
		}
	}

	var empty, err = s.Recommendations().RecommendationStats(ctx, "s", "nobody", since)
	if err != nil || empty.MeasuringSince != nil || empty.Offered != 0 || empty.RequestRate != 0 {
		t.Errorf("stats for an unknown user = %+v err=%v, want zero values", empty, err)
	}
}
