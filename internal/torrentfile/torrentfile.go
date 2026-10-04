// Package torrentfile extracts the BitTorrent v1 info hash from a .torrent file
// with a small bounded bencode scanner.
package torrentfile

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
)

// MaxSize bounds the .torrent data accepted. Real torrents with thousands of
// files stay far below this.
const MaxSize = 4 << 20

// maxDepth bounds container nesting so hostile input cannot exhaust the stack.
const maxDepth = 64

// InfoHash validates data as a bencoded v1 (or hybrid) .torrent file and
// returns the lowercase hex SHA-1 of the exact raw bytes of its info
// dictionary.
func InfoHash(data []byte) (string, error) {
	var infoStart, infoEnd = -1, -1
	var pos int

	if len(data) > MaxSize {
		return "", fmt.Errorf("torrent file exceeds %d bytes", MaxSize)
	}
	if len(data) == 0 || data[0] != 'd' {
		return "", errors.New("torrent file is not a bencoded dictionary")
	}
	pos = 1
	for {
		if pos >= len(data) {
			return "", errors.New("torrent file is truncated")
		}
		if data[pos] == 'e' {
			pos++
			break
		}
		key, next, err := readString(data, pos)
		if err != nil {
			return "", err
		}
		end, err := skip(data, next, 1)
		if err != nil {
			return "", err
		}
		if string(key) == "info" {
			if infoStart >= 0 {
				return "", errors.New("torrent file has duplicate info keys")
			}
			if data[next] != 'd' {
				return "", errors.New("torrent info is not a dictionary")
			}
			infoStart, infoEnd = next, end
		}
		pos = end
	}
	if pos != len(data) {
		return "", errors.New("torrent file has trailing data")
	}
	if infoStart < 0 {
		return "", errors.New("torrent file has no info dictionary")
	}
	hasPieces, err := dictHasKey(data[infoStart:infoEnd], "pieces")
	if err != nil {
		return "", err
	}
	if !hasPieces {
		return "", errors.New("v2-only torrents are not supported")
	}
	var sum = sha1.Sum(data[infoStart:infoEnd])
	return hex.EncodeToString(sum[:]), nil
}

// dictHasKey reports whether the already validated dictionary in data has a
// string value under key.
func dictHasKey(data []byte, key string) (bool, error) {
	var pos = 1
	var found bool

	for data[pos] != 'e' {
		name, next, err := readString(data, pos)
		if err != nil {
			return false, err
		}
		if string(name) == key {
			if data[next] < '0' || data[next] > '9' {
				return false, fmt.Errorf("torrent info %q is not a string", key)
			}
			found = true
		}
		if pos, err = skip(data, next, 1); err != nil {
			return false, err
		}
	}
	return found, nil
}

// readString reads a bencoded byte string at pos and returns its content and
// the position after it.
func readString(data []byte, pos int) ([]byte, int, error) {
	var length int
	var digits int

	for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
		length = length*10 + int(data[pos]-'0')
		digits++
		pos++
		if digits > 8 || length > len(data) {
			return nil, 0, errors.New("invalid bencode string length")
		}
	}
	if digits == 0 || pos >= len(data) || data[pos] != ':' {
		return nil, 0, errors.New("invalid bencode string")
	}
	pos++
	if length > len(data)-pos {
		return nil, 0, errors.New("bencode string is truncated")
	}
	return data[pos : pos+length], pos + length, nil
}

// skip validates the bencoded value at pos and returns the position after it.
// depth counts the containers currently open.
func skip(data []byte, pos, depth int) (int, error) {
	var err error

	if pos >= len(data) {
		return 0, errors.New("torrent file is truncated")
	}
	switch c := data[pos]; {
	case c == 'i':
		var p = pos + 1
		var first int

		if p < len(data) && data[p] == '-' {
			p++
		}
		first = p
		for p < len(data) && data[p] >= '0' && data[p] <= '9' {
			p++
		}
		if p == first || p >= len(data) || data[p] != 'e' || (p-first > 1 && data[first] == '0') {
			return 0, errors.New("invalid bencode integer")
		}
		return p + 1, nil
	case c >= '0' && c <= '9':
		_, end, err := readString(data, pos)
		return end, err
	case c == 'l' || c == 'd':
		if depth > maxDepth {
			return 0, errors.New("torrent file is nested too deeply")
		}
		pos++
		for {
			if pos >= len(data) {
				return 0, errors.New("torrent file is truncated")
			}
			if data[pos] == 'e' {
				return pos + 1, nil
			}
			if c == 'd' {
				if _, pos, err = readString(data, pos); err != nil {
					return 0, err
				}
			}
			if pos, err = skip(data, pos, depth+1); err != nil {
				return 0, err
			}
		}
	}
	return 0, errors.New("invalid bencode value")
}
