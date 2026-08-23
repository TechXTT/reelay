package tpb

import (
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeInfoHash(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{
			name: "hex is lowercased",
			in:   "24F8D55D8B3F94E28F53E1D8AE821836BC69BE99",
			want: "24f8d55d8b3f94e28f53e1d8ae821836bc69be99",
		},
		{
			name: "already lowercase",
			in:   "24f8d55d8b3f94e28f53e1d8ae821836bc69be99",
			want: "24f8d55d8b3f94e28f53e1d8ae821836bc69be99",
		},
		{
			name: "surrounding whitespace",
			in:   "  24F8D55D8B3F94E28F53E1D8AE821836BC69BE99  ",
			want: "24f8d55d8b3f94e28f53e1d8ae821836bc69be99",
		},
		{
			// Same 20 bytes, base32-encoded. qBittorrent keys on the hex form,
			// so getting this conversion wrong means every grab looks stalled:
			// the status loop polls for a hash the client never registered.
			name: "base32 converts to hex",
			in:   "ET4NKXMLH6KOFD2T4HMK5AQYG26GTPUZ",
			want: "24f8d55d8b3f94e28f53e1d8ae821836bc69be99",
		},
		{
			name: "lowercase base32",
			in:   "et4nkxmlh6kofd2t4hmk5aqyg26gtpuz",
			want: "24f8d55d8b3f94e28f53e1d8ae821836bc69be99",
		},
		{name: "empty", in: "", wantErr: "empty info hash"},
		{name: "too short", in: "abc123", wantErr: "length 6"},
		{
			name:    "40 chars but not hex",
			in:      "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
			wantErr: "not hex",
		},
		{
			name:    "32 chars but not base32",
			in:      "11111111111111111111111111111111",
			wantErr: "not base32",
		},
		{
			name:    "v2 hash is rejected explicitly",
			in:      strings.Repeat("a", 64),
			wantErr: "v2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeInfoHash(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q should contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Hex and base32 forms of one torrent must normalise to the same key, or the
// same release grabbed twice looks like two different torrents.
func TestHexAndBase32AgreeOnTheSameTorrent(t *testing.T) {
	hex, err := NormalizeInfoHash("C6D74B89F979C9BB0ED236B899DCFEFA1492D16A")
	if err != nil {
		t.Fatal(err)
	}
	b32, err := NormalizeInfoHash("Y3LUXCPZPHE3WDWSG24JTXH67IKJFULK")
	if err != nil {
		t.Fatal(err)
	}
	if hex != b32 {
		t.Errorf("hex %q and base32 %q describe the same torrent but normalised differently", hex, b32)
	}
}

func TestBuildMagnet(t *testing.T) {
	trackers := []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.demonii.com:1337/announce",
		"   ", // blank entries in config must be skipped, not emitted
	}
	magnet, err := BuildMagnet(
		"24F8D55D8B3F94E28F53E1D8AE821836BC69BE99",
		"The Expanse S01E01 1080p BluRay x264-ROVERS",
		trackers)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:24f8d55d8b3f94e28f53e1d8ae821836bc69be99") {
		t.Errorf("magnet does not start with the lowercase hex hash: %q", magnet)
	}

	q, err := url.ParseQuery(strings.TrimPrefix(magnet, "magnet:?"))
	if err != nil {
		t.Fatalf("magnet query is not parseable: %v", err)
	}
	if got := q.Get("dn"); got != "The Expanse S01E01 1080p BluRay x264-ROVERS" {
		t.Errorf("dn = %q, want the display name round-tripped", got)
	}
	if got := q["tr"]; len(got) != 2 {
		t.Errorf("tr = %v, want exactly the two non-blank trackers", got)
	}
}

func TestBuildMagnetWithoutTrackers(t *testing.T) {
	magnet, err := BuildMagnet("24F8D55D8B3F94E28F53E1D8AE821836BC69BE99", "Name", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(magnet, "&tr=") {
		t.Errorf("no trackers configured, but the magnet carries one: %q", magnet)
	}
}

func TestBuildMagnetRejectsBadHash(t *testing.T) {
	if _, err := BuildMagnet("nope", "Name", nil); err == nil {
		t.Error("expected an unusable hash to be rejected")
	}
}

func TestInfoHashFromMagnet(t *testing.T) {
	want := "24f8d55d8b3f94e28f53e1d8ae821836bc69be99"
	cases := []struct {
		name    string
		magnet  string
		want    string
		wantErr string
	}{
		{
			name:   "hex with trackers",
			magnet: "magnet:?xt=urn:btih:24F8D55D8B3F94E28F53E1D8AE821836BC69BE99&dn=Thing&tr=udp%3A%2F%2Fx%3A1337",
			want:   want,
		},
		{
			name:   "base32",
			magnet: "magnet:?xt=urn:btih:ET4NKXMLH6KOFD2T4HMK5AQYG26GTPUZ&dn=Thing",
			want:   want,
		},
		{
			name:   "uppercase scheme and urn",
			magnet: "MAGNET:?xt=URN:BTIH:24f8d55d8b3f94e28f53e1d8ae821836bc69be99",
			want:   want,
		},
		{
			name:   "hybrid v1+v2 picks the v1 hash",
			magnet: "magnet:?xt=urn:btih:24f8d55d8b3f94e28f53e1d8ae821836bc69be99&xt=urn:btmh:1220caf1e1c30e81",
			want:   want,
		},
		{
			name:    "not a magnet",
			magnet:  "http://example.com/x.torrent",
			wantErr: "not a magnet",
		},
		{
			name:    "no info hash",
			magnet:  "magnet:?dn=Thing&tr=udp%3A%2F%2Fx",
			wantErr: "no urn:btih",
		},
		{
			name:    "v2 only is rejected with a specific message",
			magnet:  "magnet:?xt=urn:btmh:1220caf1e1c30e81&dn=Thing",
			wantErr: "v2-only",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := InfoHashFromMagnet(tc.magnet)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The round trip is what phase 5 relies on: qBittorrent's add endpoint does not
// return a hash, so the key comes from the magnet we sent.
func TestMagnetRoundTrip(t *testing.T) {
	const hash = "c6d74b89f979c9bb0ed236b899dcfefa1492d16a"
	magnet, err := BuildMagnet(hash, "Some Release Name (2024) [1080p]", []string{"udp://t:1337/announce"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := InfoHashFromMagnet(magnet)
	if err != nil {
		t.Fatal(err)
	}
	if got != hash {
		t.Errorf("round trip gave %q, want %q", got, hash)
	}
}
