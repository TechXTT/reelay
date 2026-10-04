package qbittorrent

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"testing"

	"github.com/TechXTT/reelay/internal/downloader"
)

const fileTestInfo = "d6:lengthi12345e4:name8:test.mkv12:piece lengthi16384e6:pieces20:01234567890123456789e"

func TestAddTorrentFileSendsTorrentsPartAndComputesHash(t *testing.T) {
	var fake, srv = newFakeQB(t, fixtureBody(t))
	var c = newClient(t, testCfg(srv.URL))
	var file = []byte("d4:info" + fileTestInfo + "e")
	var sum = sha1.Sum([]byte(fileTestInfo))

	hash, err := c.Add(context.Background(), downloader.AddRequest{TorrentFile: file, Category: "reelay-tv",
		SavePath: "/downloads", Paused: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if hash != hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %q", hash)
	}
	fake.addMu.Lock()
	fields := fake.LastAdd
	fake.addMu.Unlock()
	if fields["torrents"] != string(file) {
		t.Errorf("torrents part = %q", fields["torrents"])
	}
	if _, ok := fields["urls"]; ok {
		t.Error("urls must not be sent with a torrent file")
	}
	if fields["category"] != "reelay-tv" || fields["savepath"] != "/downloads" ||
		fields["paused"] != "true" || fields["stopped"] != "true" {
		t.Errorf("fields = %v", fields)
	}
}

func TestAddRequiresExactlyOneSource(t *testing.T) {
	var _, srv = newFakeQB(t, fixtureBody(t))
	var c = newClient(t, testCfg(srv.URL))
	var file = []byte("d4:info" + fileTestInfo + "e")

	if _, err := c.Add(context.Background(), downloader.AddRequest{Category: "reelay-tv"}); err == nil {
		t.Error("empty request accepted")
	}
	if _, err := c.Add(context.Background(), downloader.AddRequest{Magnet: testMagnet, TorrentFile: file, Category: "reelay-tv"}); err == nil {
		t.Error("both sources accepted")
	}
	if _, err := c.Add(context.Background(), downloader.AddRequest{TorrentFile: file, Category: "foreign"}); err == nil {
		t.Error("foreign category accepted for a torrent file")
	}
	if _, err := c.Add(context.Background(), downloader.AddRequest{TorrentFile: []byte("not bencode"), Category: "reelay-tv"}); err == nil {
		t.Error("invalid torrent file accepted")
	}
}
