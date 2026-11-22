package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/engine"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

type upgradeAPIFixture struct {
	srv     *Server
	st      *store.Store
	profile model.QualityProfile
}

func newUpgradeAPIFixture(t *testing.T, interval time.Duration, maxGrabs int) *upgradeAPIFixture {
	t.Helper()
	srv, st := newTestServer(t, func(c *config.Config) {
		c.Schedules.UpgradeInterval = config.Dur(interval)
		c.Downloader.MaxUpgradeGrabs = maxGrabs
	})
	eng, err := engine.New(engine.Options{Store: st, Config: srv.cfg, Downloader: &removalDownloader{}})
	if err != nil {
		t.Fatal(err)
	}
	srv.engine = eng
	profile, err := st.Profiles().Create(context.Background(), model.QualityProfile{Name: "Upgrader", IsDefault: true,
		AllowedResolutions: []string{"1080p", "720p"}, AllowedSources: []string{"webdl"}, MinSeeders: 1,
		UpgradeUntil: "1080p", PreferredGroups: map[string]int{}})
	if err != nil {
		t.Fatal(err)
	}
	return &upgradeAPIFixture{srv: srv, st: st, profile: profile}
}

func (fx *upgradeAPIFixture) movie(t *testing.T, title, quality string) model.Movie {
	t.Helper()
	movie, err := fx.st.Movies().Create(context.Background(), model.Movie{Title: title, Year: 2020,
		ProfileID: fx.profile.ID, RootFolder: "/movies", State: model.StateImported,
		ImportedPath: "/movies/" + title + ".mkv", ImportedQuality: quality}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return movie
}

func upgradeBody(t *testing.T, fx *upgradeAPIFixture, method, path string) (int, map[string]any) {
	t.Helper()
	rec := do(t, fx.srv.Handler(), method, path, authed())
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s body %q: %v", method, path, rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestUpgradesListShapeAndPagination(t *testing.T) {
	fx := newUpgradeAPIFixture(t, 168*time.Hour, 2)
	fx.movie(t, "Alpha", "720p webdl")
	fx.movie(t, "Bravo", "720p webdl")
	fx.movie(t, "Charlie", "1080p webdl")

	code, body := upgradeBody(t, fx, http.MethodGet, "/api/v1/upgrades")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if body["enabled"] != true || body["max_upgrade_grabs"] != float64(2) || body["inflight"] != float64(0) ||
		body["total"] != float64(2) || body["limit"] != float64(100) || body["has_more"] != false {
		t.Errorf("envelope = %v", body)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want the two movies below cutoff", items)
	}
	first := items[0].(map[string]any)
	for key, want := range map[string]any{"subject_type": "movie", "title": "Alpha", "quality": "720p webdl",
		"cutoff": "1080p", "profile": "Upgrader", "state": "imported", "upgrading": false} {
		if first[key] != want {
			t.Errorf("first[%q] = %v, want %v", key, first[key], want)
		}
	}
	if _, ok := first["id"]; !ok {
		t.Error("first has no id")
	}
	if _, present := first["upgrade_checked_at"]; present {
		t.Error("an unchecked item must omit upgrade_checked_at")
	}

	code, body = upgradeBody(t, fx, http.MethodGet, "/api/v1/upgrades?limit=1&offset=1")
	items = body["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["title"] != "Bravo" || body["has_more"] != false {
		t.Errorf("page 2 = %d %v", code, body)
	}
	_, body = upgradeBody(t, fx, http.MethodGet, "/api/v1/upgrades?limit=1")
	if body["has_more"] != true {
		t.Errorf("page 1 has_more = %v, want true", body["has_more"])
	}
	for _, query := range []string{"limit=0", "limit=501", "limit=x", "offset=-1"} {
		if code, _ := upgradeBody(t, fx, http.MethodGet, "/api/v1/upgrades?"+query); code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, code)
		}
	}
}

func TestUpgradesListWhenDisabledStillReportsSettings(t *testing.T) {
	fx := newUpgradeAPIFixture(t, 0, 0)
	code, body := upgradeBody(t, fx, http.MethodGet, "/api/v1/upgrades")
	if code != http.StatusOK || body["enabled"] != false || len(body["items"].([]any)) != 0 {
		t.Errorf("disabled list = %d %v", code, body)
	}
}

func TestUpgradeSearchStartsAndRefuses(t *testing.T) {
	fx := newUpgradeAPIFixture(t, 168*time.Hour, 1)
	below := fx.movie(t, "Below", "720p webdl")
	other := fx.movie(t, "Other", "720p webdl")
	atCutoff := fx.movie(t, "AtCutoff", "1080p webdl")
	url := func(subject string, id int64) string {
		return "/api/v1/upgrades/" + subject + "/" + strconv.FormatInt(id, 10) + "/search"
	}

	cases := []struct {
		name string
		path string
		want int
	}{
		{"unknown item", url("movie", 9999), http.StatusNotFound},
		{"bad subject", url("series", below.ID), http.StatusBadRequest},
		{"bad id", "/api/v1/upgrades/movie/zero/search", http.StatusBadRequest},
		{"not below cutoff", url("movie", atCutoff.ID), http.StatusConflict},
		{"success", url("movie", below.ID), http.StatusAccepted},
		{"already upgrading", url("movie", below.ID), http.StatusConflict},
		{"limit reached", url("movie", other.ID), http.StatusConflict},
	}
	for _, tc := range cases {
		if code, body := upgradeBody(t, fx, http.MethodPost, tc.path); code != tc.want {
			t.Errorf("%s: status = %d, want %d (%v)", tc.name, code, tc.want, body)
		}
	}
	if got, err := fx.st.Movies().Get(context.Background(), below.ID); err != nil || got.State != model.StateWanted {
		t.Errorf("started movie = %+v, %v; want wanted", got.State, err)
	}
	_, body := upgradeBody(t, fx, http.MethodPost, url("movie", other.ID))
	if message := body["error"].(map[string]any)["message"]; message != "upgrade limit reached" {
		t.Errorf("limit message = %v", message)
	}
	_, list := upgradeBody(t, fx, http.MethodGet, "/api/v1/upgrades")
	if list["inflight"] != float64(1) || list["items"].([]any)[0].(map[string]any)["upgrading"] != true {
		t.Errorf("list after start = %v", list)
	}
}

func TestUpgradeSearchWhenDisabledIsAConflict(t *testing.T) {
	fx := newUpgradeAPIFixture(t, 0, 0)
	movie := fx.movie(t, "Below", "720p webdl")
	code, body := upgradeBody(t, fx, http.MethodPost, "/api/v1/upgrades/movie/"+strconv.FormatInt(movie.ID, 10)+"/search")
	if code != http.StatusConflict || body["error"].(map[string]any)["message"] != "upgrades are disabled" {
		t.Errorf("disabled start = %d %v", code, body)
	}
}
