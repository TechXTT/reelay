package tpb

import (
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// BuildMagnet assembles a magnet link from an info hash, a display name and a
// tracker list.
//
// The API gives no magnet link, only the hash, so this is the only way to hand
// anything to the download client. The tracker list comes from config, not from
// code: a magnet with no trackers relies entirely on DHT and can sit at zero
// peers indefinitely, and which trackers are alive changes over time.
func BuildMagnet(infoHash, displayName string, trackers []string) (string, error) {
	h, err := NormalizeInfoHash(infoHash)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(h)
	if displayName != "" {
		b.WriteString("&dn=")
		b.WriteString(url.QueryEscape(displayName))
	}
	for _, tr := range trackers {
		if tr = strings.TrimSpace(tr); tr != "" {
			b.WriteString("&tr=")
			b.WriteString(url.QueryEscape(tr))
		}
	}
	return b.String(), nil
}

// NormalizeInfoHash returns a lowercase 40-character hex v1 info hash.
//
// Base32 is accepted and converted: some indexers and most magnet links found
// in the wild use the 32-character base32 encoding of the same 20 bytes, and
// qBittorrent keys torrents by the hex form. Mixing the two representations
// means the status loop polls for a hash the client has never heard of and
// every grab looks stalled.
func NormalizeInfoHash(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty info hash")
	}

	switch len(s) {
	case 40:
		lower := strings.ToLower(s)
		if _, err := hex.DecodeString(lower); err != nil {
			return "", fmt.Errorf("info hash %q is 40 characters but not hex: %w", s, err)
		}
		return lower, nil

	case 32:
		// base32 as used in magnet links: uppercase, unpadded.
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).
			DecodeString(strings.ToUpper(s))
		if err != nil {
			return "", fmt.Errorf("info hash %q is 32 characters but not base32: %w", s, err)
		}
		if len(raw) != 20 {
			return "", fmt.Errorf("info hash %q decoded to %d bytes, want 20", s, len(raw))
		}
		return hex.EncodeToString(raw), nil

	case 64:
		// A v2 (SHA-256) hash. Reelay does not support v2-only torrents: the
		// download client keys on the v1 hash, and a truncated v2 hash is not
		// the v1 hash of the same torrent.
		return "", fmt.Errorf("info hash %q looks like a BitTorrent v2 hash; v2-only torrents are not supported", s)

	default:
		return "", fmt.Errorf("info hash %q has length %d, want 40 (hex) or 32 (base32)", s, len(s))
	}
}

// InfoHashFromMagnet extracts and normalises the v1 info hash from a magnet URI.
//
// Needed because qBittorrent's add endpoint does not return the hash of what it
// just added, so the hash has to be computed on this side and used as the key.
func InfoHashFromMagnet(magnet string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		return "", fmt.Errorf("not a magnet URI: %.32q", magnet)
	}
	// Parse the query directly: url.Parse puts everything after "magnet:" in
	// Opaque, and the fragment-free query is not exposed as RawQuery.
	q := magnet
	if i := strings.Index(q, "?"); i >= 0 {
		q = q[i+1:]
	}
	values, err := url.ParseQuery(q)
	if err != nil {
		return "", fmt.Errorf("parse magnet query: %w", err)
	}

	for _, xt := range values["xt"] {
		const prefix = "urn:btih:"
		if len(xt) > len(prefix) && strings.EqualFold(xt[:len(prefix)], prefix) {
			return NormalizeInfoHash(xt[len(prefix):])
		}
	}
	// A v2-only magnet uses urn:btmh; say so specifically rather than
	// reporting a generic parse failure.
	for _, xt := range values["xt"] {
		if strings.HasPrefix(strings.ToLower(xt), "urn:btmh:") {
			return "", fmt.Errorf("magnet carries only a v2 (btmh) hash; v2-only torrents are not supported")
		}
	}
	return "", fmt.Errorf("magnet has no urn:btih info hash")
}
