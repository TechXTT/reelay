package torrentfile

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
)

const testInfo = "d6:lengthi12345e4:name8:test.mkv12:piece lengthi16384e6:pieces20:01234567890123456789e"

func buildTorrent(info string) []byte {
	return []byte("d8:announce20:http://tracker/x/ann4:info" + info + "e")
}

func TestInfoHashKnownTorrent(t *testing.T) {
	var sum = sha1.Sum([]byte(testInfo))
	var want = hex.EncodeToString(sum[:])

	got, err := InfoHash(buildTorrent(testInfo))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
}

func TestInfoHashInfoNotFirstKey(t *testing.T) {
	var data = []byte("d4:info" + testInfo + "7:comment2:hie")
	var sum = sha1.Sum([]byte(testInfo))

	got, err := InfoHash(data)
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash=%s err=%v", got, err)
	}
}

func TestInfoHashHybridUsesRawInfoBytes(t *testing.T) {
	var info = "d9:file treed4:name1:xe4:name1:n12:piece lengthi1e6:pieces0:12:meta versioni2ee"
	var sum = sha1.Sum([]byte(info))

	got, err := InfoHash(buildTorrent(info))
	if err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash=%s err=%v", got, err)
	}
}

func TestInfoHashRejectsV2Only(t *testing.T) {
	var info = "d9:file treed4:name1:xe4:name1:n12:meta versioni2ee"

	_, err := InfoHash(buildTorrent(info))
	if err == nil || err.Error() != "v2-only torrents are not supported" {
		t.Fatalf("err = %v", err)
	}
}

func TestInfoHashRejectsMalformed(t *testing.T) {
	var deep = strings.Repeat("l", 70) + strings.Repeat("e", 70)
	var cases = map[string][]byte{
		"empty":            nil,
		"not a dict":       []byte("l4:infoe"),
		"text":             []byte("<html>not a torrent</html>"),
		"truncated":        []byte("d4:info" + testInfo[:20]),
		"missing end":      []byte("d4:info" + testInfo),
		"no info":          []byte("d8:announce3:fooe"),
		"info not dict":    []byte("d4:info3:fooe"),
		"info is list":     []byte("d4:infolee"),
		"trailing garbage": append(buildTorrent(testInfo), 'x'),
		"trailing dict":    append(buildTorrent(testInfo), "de"...),
		"duplicate info":   []byte("d4:info" + testInfo + "4:info" + testInfo + "e"),
		"bad integer":      []byte("d1:ai-e4:info" + testInfo + "e"),
		"leading zero":     []byte("d1:ai01e4:info" + testInfo + "e"),
		"huge string":      []byte("d99999999:x4:info" + testInfo + "e"),
		"string too long":  []byte("d4:info" + testInfo[:10] + "50:abc" + "ee"),
		"non string key":   []byte("di1e1:ae"),
		"bad value":        []byte("d1:a?e"),
		"too deep":         []byte("d1:a" + deep + "4:info" + testInfo + "e"),
		"pieces not str":   buildTorrent("d6:piecesi1ee"),
	}

	for name, data := range cases {
		if _, err := InfoHash(data); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestInfoHashDepthLimitBoundary(t *testing.T) {
	var ok = strings.Repeat("l", maxDepth-1) + strings.Repeat("e", maxDepth-1)
	var tooDeep = strings.Repeat("l", maxDepth+1) + strings.Repeat("e", maxDepth+1)

	if _, err := InfoHash([]byte("d1:a" + ok + "4:info" + testInfo + "e")); err != nil {
		t.Fatalf("allowed depth rejected: %v", err)
	}
	if _, err := InfoHash([]byte("d1:a" + tooDeep + "4:info" + testInfo + "e")); err == nil {
		t.Fatal("excess depth accepted")
	}
}

func TestInfoHashRejectsOversize(t *testing.T) {
	var big = append([]byte("d1:a"), bytes.Repeat([]byte("1"), MaxSize)...)

	if _, err := InfoHash(big); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v", err)
	}
}
