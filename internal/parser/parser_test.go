package parser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// expectation mirrors Parsed with pointer fields, so a case asserts only what
// it lists. Absent means "not checked", which is what makes a 170-case corpus
// maintainable — each case stays about the one thing it exists to pin down.
type expectation struct {
	Title        *string   `json:"title"`
	TitleRaw     *string   `json:"title_raw"`
	Year         *int      `json:"year"`
	Season       *int      `json:"season"`
	Episodes     *[]int    `json:"episodes"`
	SeasonEnd    *int      `json:"season_end"`
	IsSeasonPack *bool     `json:"is_season_pack"`
	AbsoluteEp   *int      `json:"absolute_ep"`
	AirDate      *string   `json:"air_date"`
	Resolution   *string   `json:"resolution"`
	Source       *string   `json:"source"`
	VideoCodec   *string   `json:"video_codec"`
	AudioCodec   *string   `json:"audio_codec"`
	HDR          *[]string `json:"hdr"`
	Language     *[]string `json:"language"`
	ReleaseGroup *string   `json:"release_group"`
	Proper       *bool     `json:"proper"`
	Repack       *bool     `json:"repack"`
	Container    *string   `json:"container"`
	Truncated    *bool     `json:"truncated"`
}

type corpusCase struct {
	Group        string      `json:"group"`
	Name         string      `json:"name"`
	Expect       expectation `json:"expect"`
	Note         string      `json:"note"`
	Pathological bool        `json:"pathological"`
}

type corpus struct {
	Cases []corpusCase `json:"cases"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "releases.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c corpus
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&struct {
		Comment []string      `json:"_comment"`
		Cases   *[]corpusCase `json:"cases"`
	}{Cases: &c.Cases}); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	return c
}

// TestGoldenCorpus is the parser's definition of done. If this is red, the
// parser is not finished, whatever else passes.
func TestGoldenCorpus(t *testing.T) {
	c := loadCorpus(t)

	// The spec sets a floor of 150 real-world names. Guard it, so the corpus
	// cannot quietly shrink.
	if len(c.Cases) < 150 {
		t.Fatalf("corpus has %d cases, want at least 150", len(c.Cases))
	}
	pathological := 0
	for _, tc := range c.Cases {
		if tc.Pathological {
			pathological++
		}
	}
	if pathological < 10 {
		t.Errorf("corpus has %d pathological cases, want at least 10", pathological)
	}

	for i, tc := range c.Cases {
		name := fmt.Sprintf("%03d/%s", i, truncateForName(tc.Name))
		t.Run(name, func(t *testing.T) {
			got := Parse(tc.Name)
			checkExpectation(t, tc, got)
		})
	}
}

func checkExpectation(t *testing.T, tc corpusCase, got Parsed) {
	t.Helper()
	e := tc.Expect

	fail := func(field string, want, have any) {
		t.Helper()
		msg := fmt.Sprintf("%s:\n  name: %q\n  want %s = %v\n  got  %s = %v",
			field, tc.Name, field, want, field, have)
		if tc.Note != "" {
			msg += "\n  note: " + tc.Note
		}
		t.Error(msg)
	}

	if e.Title != nil && got.Title != *e.Title {
		fail("title", *e.Title, got.Title)
	}
	if e.TitleRaw != nil && got.TitleRaw != *e.TitleRaw {
		fail("title_raw", *e.TitleRaw, got.TitleRaw)
	}
	if e.Year != nil && got.Year != *e.Year {
		fail("year", *e.Year, got.Year)
	}
	if e.Season != nil && got.Season != *e.Season {
		fail("season", *e.Season, got.Season)
	}
	if e.Episodes != nil && !equalInts(got.Episodes, *e.Episodes) {
		fail("episodes", *e.Episodes, got.Episodes)
	}
	if e.SeasonEnd != nil && got.SeasonEnd != *e.SeasonEnd {
		fail("season_end", *e.SeasonEnd, got.SeasonEnd)
	}
	if e.IsSeasonPack != nil && got.IsSeasonPack != *e.IsSeasonPack {
		fail("is_season_pack", *e.IsSeasonPack, got.IsSeasonPack)
	}
	if e.AbsoluteEp != nil && got.AbsoluteEp != *e.AbsoluteEp {
		fail("absolute_ep", *e.AbsoluteEp, got.AbsoluteEp)
	}
	if e.AirDate != nil && got.AirDate != *e.AirDate {
		fail("air_date", *e.AirDate, got.AirDate)
	}
	if e.Resolution != nil && got.Resolution != *e.Resolution {
		fail("resolution", *e.Resolution, got.Resolution)
	}
	if e.Source != nil && got.Source != *e.Source {
		fail("source", *e.Source, got.Source)
	}
	if e.VideoCodec != nil && got.VideoCodec != *e.VideoCodec {
		fail("video_codec", *e.VideoCodec, got.VideoCodec)
	}
	if e.AudioCodec != nil && got.AudioCodec != *e.AudioCodec {
		fail("audio_codec", *e.AudioCodec, got.AudioCodec)
	}
	if e.HDR != nil && !equalStrings(got.HDR, *e.HDR) {
		fail("hdr", *e.HDR, got.HDR)
	}
	if e.Language != nil && !equalStrings(got.Language, *e.Language) {
		fail("language", *e.Language, got.Language)
	}
	if e.ReleaseGroup != nil && got.ReleaseGroup != *e.ReleaseGroup {
		fail("release_group", *e.ReleaseGroup, got.ReleaseGroup)
	}
	if e.Proper != nil && got.Proper != *e.Proper {
		fail("proper", *e.Proper, got.Proper)
	}
	if e.Repack != nil && got.Repack != *e.Repack {
		fail("repack", *e.Repack, got.Repack)
	}
	if e.Container != nil && got.Container != *e.Container {
		fail("container", *e.Container, got.Container)
	}
	if e.Truncated != nil && got.Truncated != *e.Truncated {
		fail("truncated", *e.Truncated, got.Truncated)
	}
}

// Parse must be free of side effects on its input and stable across calls: the
// engine parses the same release name from several loops.
func TestParseIsDeterministic(t *testing.T) {
	c := loadCorpus(t)
	for _, tc := range c.Cases {
		first := Parse(tc.Name)
		second := Parse(tc.Name)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("Parse(%q) is not deterministic:\n  %+v\n  %+v", tc.Name, first, second)
		}
	}
}

// Every case must survive a JSON round trip, because Parsed is persisted in the
// releases table as parsed_json and read back by the importer.
func TestParsedRoundTripsThroughJSON(t *testing.T) {
	c := loadCorpus(t)
	for _, tc := range c.Cases {
		want := Parse(tc.Name)
		blob, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal %q: %v", tc.Name, err)
		}
		var got Parsed
		if err := json.Unmarshal(blob, &got); err != nil {
			t.Fatalf("unmarshal %q: %v", tc.Name, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Errorf("round trip changed %q:\n  before %+v\n  after  %+v", tc.Name, want, got)
		}
	}
}

func TestCoversEpisode(t *testing.T) {
	cases := []struct {
		name        string
		release     string
		season, ep  int
		wantCovered bool
	}{
		{"exact", "Show.S01E05.1080p.WEB-DL.x264", 1, 5, true},
		{"wrong episode", "Show.S01E05.1080p.WEB-DL.x264", 1, 6, false},
		{"wrong season", "Show.S01E05.1080p.WEB-DL.x264", 2, 5, false},
		{"multi-episode covers middle", "Show.S01E01-E03.1080p.WEB-DL.x264", 1, 2, true},
		{"season pack covers anything in season", "Show.S01.1080p.BluRay.x265", 1, 17, true},
		{"season pack rejects other season", "Show.S01.1080p.BluRay.x265", 2, 1, false},
		{"multi-season pack covers middle season", "Show.S01-S03.1080p.BluRay.x264", 2, 4, true},
		{"multi-season pack rejects outside", "Show.S01-S03.1080p.BluRay.x264", 4, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Parse(tc.release)
			if got := p.CoversEpisode(tc.season, tc.ep); got != tc.wantCovered {
				t.Errorf("CoversEpisode(%d, %d) = %v, want %v (parsed %+v)",
					tc.season, tc.ep, got, tc.wantCovered, p)
			}
		})
	}
}

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"The Expanse":             "the expanse",
		"The.Expanse":             "the expanse",
		"Marvel's Daredevil":      "marvels daredevil",
		"Spider-Man: No Way Home": "spider man no way home",
		"Tom & Jerry":             "tom and jerry",
		"S.W.A.T.":                "s w a t",
		"  Extra   Spaces  ":      "extra spaces",
		"Ocean's Eleven":          "oceans eleven",
		"Se7en":                   "se7en",
		"Léon":                    "léon",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeForMatchStripsArticle(t *testing.T) {
	cases := map[string]string{
		"The Expanse":   "expanse",
		"A Quiet Place": "quiet place",
		"An Education":  "education",
		"Theodore":      "theodore",
	}
	for in, want := range cases {
		if got := NormalizeForMatch(in); got != want {
			t.Errorf("NormalizeForMatch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSortTitle(t *testing.T) {
	cases := map[string]string{
		"The Expanse":   "expanse, the",
		"A Quiet Place": "quiet place, a",
		"Breaking Bad":  "breaking bad",
	}
	for in, want := range cases {
		if got := SortTitle(in); got != want {
			t.Errorf("SortTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		max  int
		want int
	}{
		{"", "", 2, 0},
		{"abc", "abc", 2, 0},
		{"abc", "abd", 2, 1},
		{"kitten", "sitting", 3, 3},
		{"expanse", "expanse", 2, 0},
		{"the expanse", "the expance", 2, 1},
		// Bounded: a wildly different string returns max+1 rather than the
		// true distance, which is all the caller needs.
		{"abc", "zzzzzzzzzzzzzz", 2, 3},
	}
	for _, tc := range cases {
		if got := Levenshtein(tc.a, tc.b, tc.max); got != tc.want {
			t.Errorf("Levenshtein(%q, %q, %d) = %d, want %d", tc.a, tc.b, tc.max, got, tc.want)
		}
	}
}

// A flat distance-2 budget would make these match. Short titles get no
// tolerance precisely because one edit is the difference between two real,
// unrelated titles.
func TestFuzzyBudgetIsZeroForShortTitles(t *testing.T) {
	for _, title := range []string{"us", "up", "her", "it", "alone", "dune"} {
		if b := fuzzyBudget(len(title)); b != 0 {
			t.Errorf("fuzzyBudget(%q) = %d, want 0 — short titles must be exact", title, b)
		}
	}
	if b := fuzzyBudget(len("the expanse")); b != 1 {
		t.Errorf("fuzzyBudget(11) = %d, want 1", b)
	}
	if b := fuzzyBudget(len("everything everywhere all at once")); b != 2 {
		t.Errorf("fuzzyBudget(33) = %d, want 2", b)
	}
}

func TestParseNeverPanics(t *testing.T) {
	// Inputs chosen to poke at the index arithmetic in the boundary logic.
	inputs := []string{
		"", " ", ".", "-", "[", "]", "()", "S", "E", "S0", "E0",
		"S01E", "E01S", "x", "1x", "x1", "0x0", "----", "[[[]]]",
		"S01E01-", "S01E01-E", "-2024-", "(2024)", "((((", "))))",
		"\x00\x01\x02", "日本語", "🎬", "a" + string(make([]byte, 300)),
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Parse(%q) panicked: %v", in, r)
				}
			}()
			_ = Parse(in)
		}()
	}
}

func equalInts(a, b []int) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func truncateForName(s string) string {
	if s == "" {
		return "(empty)"
	}
	if len(s) > 48 {
		return s[:48]
	}
	return s
}
