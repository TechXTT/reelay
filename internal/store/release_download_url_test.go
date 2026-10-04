package store

import (
	"context"
	"testing"

	"github.com/TechXTT/reelay/internal/model"
)

func TestReleaseDownloadURLRoundTrips(t *testing.T) {
	var ctx = context.Background()
	var s = openTestStore(t)
	var in = model.StoredRelease{Indexer: "fixture", InfoHash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		RawTitle: "Moon.2009.1080p.BluRay.x264-GROUP", DownloadURL: "http://indexer.test/dl/1.torrent?file=a",
		SizeBytes: 1000, Seeders: 20}

	if err := Migrate(ctx, s, testLogger()); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Releases().Upsert(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DownloadURL != in.DownloadURL || stored.Magnet != "" {
		t.Fatalf("upsert returned %+v", stored)
	}
	got, err := s.Releases().Get(ctx, stored.ID)
	if err != nil || got.DownloadURL != in.DownloadURL {
		t.Fatalf("get = %+v err=%v", got, err)
	}
	byHash, err := s.Releases().ByIndexerHash(ctx, "fixture", in.InfoHash)
	if err != nil || byHash.DownloadURL != in.DownloadURL {
		t.Fatalf("by hash = %+v err=%v", byHash, err)
	}
	in.DownloadURL = "http://indexer.test/dl/2.torrent"
	if _, err := s.Releases().Upsert(ctx, in); err != nil {
		t.Fatal(err)
	}
	many, err := s.Releases().GetMany(ctx, []int64{stored.ID})
	if err != nil || many[stored.ID].DownloadURL != in.DownloadURL {
		t.Fatalf("conflict update lost the url: %+v err=%v", many, err)
	}
}
