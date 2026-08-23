package tpb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// row is one record from the Pirate Bay JSON API.
//
// Every numeric field is flexInt, and this is not defensiveness — the same
// logical record arrives in two different JSON shapes depending on the
// endpoint, verified against live responses:
//
//	q.php                              →  "seeders":"607", "size":"23598867291"
//	precompiled/data_top100_recent.json →  "seeders":0,     "size":240307327
//
// The recent endpoint also sends `imdb: null` where the search endpoint sends
// `imdb: ""`, and adds an `anon` field the search endpoint omits. A single
// struct with plain int64 fields fails to decode one endpoint or the other.
type row struct {
	ID       flexInt    `json:"id"`
	Name     string     `json:"name"`
	InfoHash string     `json:"info_hash"`
	Leechers flexInt    `json:"leechers"`
	Seeders  flexInt    `json:"seeders"`
	NumFiles flexInt    `json:"num_files"`
	Size     flexInt    `json:"size"`
	Username string     `json:"username"`
	Added    flexInt    `json:"added"`
	Status   string     `json:"status"`
	Category flexInt    `json:"category"`
	IMDB     flexString `json:"imdb"`

	// Present only on q.php, and only on the first element. Not trusted for
	// anything: it reads "1" on the no-results marker.
	TotalFound flexInt `json:"total_found"`
	// Present only on the recent endpoint.
	Anon flexInt `json:"anon"`
}

// flexInt decodes a JSON number, a quoted number, null, or an empty string.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = 0
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("flexInt: %w", err)
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			// A non-numeric string where a number belongs is bad data, not a
			// fatal error: one malformed row must not discard the response.
			*f = 0
			return nil
		}
		*f = flexInt(n)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("flexInt: %w", err)
	}
	v, err := n.Int64()
	if err != nil {
		// Some mirrors emit sizes in scientific notation.
		fl, ferr := n.Float64()
		if ferr != nil {
			*f = 0
			return nil
		}
		v = int64(fl)
	}
	*f = flexInt(v)
	return nil
}

func (f flexInt) Int() int     { return int(f) }
func (f flexInt) Int64() int64 { return int64(f) }

// flexString decodes a JSON string or null.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		// Tolerate a bare number, which some mirrors send for imdb.
		var n json.Number
		if json.Unmarshal(b, &n) == nil {
			*f = flexString(n.String())
			return nil
		}
		return fmt.Errorf("flexString: %w", err)
	}
	*f = flexString(s)
	return nil
}

func (f flexString) String() string { return string(f) }

// isNoResultsMarker reports whether rows is the API's "nothing here" response.
//
// The API does not return an empty array. It returns exactly one row with
// id 0, a zero info hash and a name along the lines of "No results returned".
// Matching on the id and hash rather than only on the name keeps this working
// if the wording changes.
func isNoResultsMarker(rows []row) bool {
	if len(rows) != 1 {
		return false
	}
	r := rows[0]
	if r.ID != 0 {
		return false
	}
	if strings.Trim(r.InfoHash, "0") == "" {
		return true
	}
	return strings.Contains(strings.ToLower(r.Name), "no results")
}

// valid rejects rows that cannot become a usable Release.
func (r row) valid() bool {
	if strings.TrimSpace(r.Name) == "" {
		return false
	}
	// A v1 info hash is 40 hex characters; a base32 one is 32. Anything else
	// cannot be turned into a magnet link.
	h := strings.TrimSpace(r.InfoHash)
	return len(h) == 40 || len(h) == 32
}
