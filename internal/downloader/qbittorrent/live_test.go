package qbittorrent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
)

// Live tests run against a real qBittorrent instead of the fake. Skipped unless
// REELAY_LIVE_QBT=1, so `go test ./...` and CI stay hermetic.
//
// They exist because the fake can only prove the client behaves correctly
// against the fake. The ownership rule in particular is worth checking against
// a real client holding the operator's own torrents, since that is the exact
// situation it protects:
//
//	REELAY_LIVE_QBT=1 REELAY_LIVE_QBT_CONFIG=../../../config.yaml go test ./internal/downloader/qbittorrent -run Live -v
func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("REELAY_LIVE_QBT") != "1" {
		t.Skip("set REELAY_LIVE_QBT=1 to run against a real qBittorrent")
	}

	path := os.Getenv("REELAY_LIVE_QBT_CONFIG")
	if path == "" {
		path = "../../../config.yaml"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("cannot read %s: %v", path, err)
	}

	// A deliberately small YAML reader rather than the real loader: the loader
	// validates library roots and download paths, which a test has no business
	// requiring to exist.
	field := func(key string) string {
		re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s*"([^"]*)"`)
		if m := re.FindStringSubmatch(string(raw)); m != nil {
			return m[1]
		}
		return ""
	}

	cfg := config.Downloader{
		Type:           "qbittorrent",
		URL:            field("url"),
		Username:       field("username"),
		Password:       field("password"),
		CategoryTV:     field("category_tv"),
		CategoryMovies: field("category_movies"),
	}
	if cfg.URL == "" || cfg.CategoryTV == "" {
		t.Skip("config has no download client url or categories")
	}

	c, err := New(cfg, Options{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Timeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestLiveHealthy(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	if err := c.Healthy(ctx); err != nil {
		t.Fatalf("Healthy: %v", err)
	}
	ver, err := c.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("qBittorrent %s, Web API %s", ver, c.APIVersion())
}

// The ownership rule, checked against a real client that holds torrents Reelay
// did not add. This is the one failure mode that would be unrecoverable: a bug
// here deletes the operator's data.
func TestLiveStatusNeverLeaksForeignTorrents(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	// Ask for everything. Whatever comes back must carry one of our categories.
	got, err := c.Status(ctx, nil)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	owned := map[string]bool{}
	for _, cat := range c.OwnedCategories() {
		owned[cat] = true
	}
	for _, s := range got {
		if !owned[s.Category] {
			t.Errorf("torrent %q (category %q) leaked through the ownership filter", s.Name, s.Category)
		}
	}
	t.Logf("client holds torrents; %d are Reelay's", len(got))
}

// Remove must refuse a torrent it does not own, and it must decide that by
// asking the client rather than by trusting the caller.
func TestLiveRemoveRefusesForeignTorrent(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	// Find a torrent that is NOT ours by going around our own filter.
	foreign, name := liveForeignHash(t, c)
	if foreign == "" {
		t.Skip("the client holds no torrents outside Reelay's categories")
	}

	err := c.Remove(ctx, foreign, true)
	if !errors.Is(err, downloader.ErrNotOurs) {
		t.Fatalf("Remove(%s %q) = %v, want ErrNotOurs — this torrent belongs to the operator",
			foreign, name, err)
	}
	t.Logf("correctly refused to remove %q (not ours)", name)

	// And it is still there.
	if still, _ := liveForeignHash(t, c); still != foreign {
		t.Error("the foreign torrent is no longer the first foreign torrent; something removed one")
	}
}

// testTorrentPrefix marks torrents this test suite created. Nothing else is
// ever a removal candidate.
const testTorrentPrefix = "Reelay.Contract.Test"

// TestLiveRemoveOwnTestTorrent exercises the positive Remove path and cleans up
// after the --grab checkpoint.
//
// Scoped hard on purpose: it will only delete a torrent that is both in one of
// Reelay's categories AND named with the test prefix. A live test that can
// delete is only acceptable if it cannot delete anything it did not create.
func TestLiveRemoveOwnTestTorrent(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	ours, err := c.Status(ctx, nil)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	removed := 0
	for _, s := range ours {
		if !strings.HasPrefix(s.Name, testTorrentPrefix) {
			continue
		}
		// deleteData false: the paused test torrent never fetched metadata, so
		// there is nothing on disk, and false is the safer default regardless.
		if err := c.Remove(ctx, s.Hash, false); err != nil {
			t.Errorf("Remove(%s %q): %v", s.Hash, s.Name, err)
			continue
		}
		t.Logf("removed our own test torrent %q", s.Name)
		removed++
	}
	if removed == 0 {
		t.Skipf("no torrent named %s* in Reelay's categories; nothing to clean up", testTorrentPrefix)
	}

	// It must actually be gone.
	after, err := c.Status(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range after {
		if strings.HasPrefix(s.Name, testTorrentPrefix) {
			t.Errorf("%q survived removal", s.Name)
		}
	}
}

// liveForeignHash reads the raw endpoint directly, bypassing Status's ownership
// filter, so the test can find something it is not allowed to touch.
func liveForeignHash(t *testing.T, c *Client) (hash, name string) {
	t.Helper()
	body, err := c.do(context.Background(), request{method: "GET", path: "/api/v2/torrents/info"})
	if err != nil {
		t.Fatalf("raw torrents/info: %v", err)
	}
	var rows []infoRow
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range rows {
		if !c.ownedCategories[r.Category] {
			return r.Hash, r.Name
		}
	}
	return "", ""
}
