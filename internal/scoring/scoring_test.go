package scoring

import (
	"strings"
	"testing"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
)

var testNow = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// defaultWeights mirrors the shipped config, so these tests exercise the
// numbers a real install actually runs with.
func defaultWeights() config.Scoring {
	return config.Scoring{
		ResolutionWeight:   1000,
		SourceWeight:       500,
		GroupWeight:        300,
		LanguageWeight:     250,
		ProperRepackWeight: 200,
		SeederWeightMax:    150,
		HDRWeight:          100,
		SeasonPackWeight:   400,
		AgePenaltyPerDay:   1,
		AgePenaltyMax:      60,
	}
}

func tvProfile() model.QualityProfile {
	return model.QualityProfile{
		Name:               "TV 1080p",
		AllowedResolutions: []string{"1080p", "720p"},
		AllowedSources:     []string{"bluray", "webdl", "webrip", "hdtv"},
		MinSizeMB:          200,
		MaxSizeMB:          12000,
		MinSeeders:         3,
		BannedTerms:        []string{"cam", "hdcam", "ts", "telesync", "screener"},
		PreferredGroups:    map[string]int{"NTb": 300, "FLUX": 300, "YIFY": -200},
		LanguagePrefs:      []string{"en"},
		UpgradeUntil:       "1080p",
	}
}

func wantEp(season, ep int, wanted ...int) *model.Wanted {
	return &model.Wanted{
		Kind:           model.SubjectEpisode,
		Title:          parser.NormalizeTitle("The Expanse"),
		Season:         season,
		Episode:        ep,
		WantedEpisodes: wanted,
	}
}

// rel builds a release. Age is expressed in days before testNow so cases read
// declaratively.
func rel(title string, seeders, sizeMB, ageDays int) indexer.Release {
	return indexer.Release{
		Title:       title,
		InfoHash:    hashFor(title),
		Magnet:      "magnet:?xt=urn:btih:" + hashFor(title),
		SizeBytes:   int64(sizeMB) * 1024 * 1024,
		Seeders:     seeders,
		Indexer:     "test",
		PublishedAt: testNow.AddDate(0, 0, -ageDays),
	}
}

// hashFor derives a stable, distinct 40-hex hash from a title so tie-breaking
// is deterministic without hand-writing hashes.
func hashFor(s string) string {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 40)
	for i := range out {
		out[i] = hexDigits[(h>>(4*(uint(i)%16)))&0xf]
		if i%16 == 15 {
			h *= 1099511628211
		}
	}
	return string(out)
}

func baseInput(releases []indexer.Release, want *model.Wanted) Input {
	return Input{
		Releases: releases,
		Want:     want,
		Profile:  tvProfile(),
		Weights:  defaultWeights(),
		Now:      testNow,
	}
}

// ---------------------------------------------------------------------------
// Stage 1: one test per rejection reason
// ---------------------------------------------------------------------------

func TestHardFilters(t *testing.T) {
	cases := []struct {
		name     string
		release  indexer.Release
		mutate   func(*Input)
		wantCat  string
		inReason string
	}{
		{
			name:     "unparseable name",
			release:  rel("..........", 50, 2000, 1),
			wantCat:  RejectUnparseable,
			inReason: "could not be parsed",
		},
		{
			name:     "wrong episode",
			release:  rel("The.Expanse.S01E05.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
			wantCat:  RejectWrongItem,
			inReason: "wanted S01E01",
		},
		{
			name:     "wrong show",
			release:  rel("The.Wire.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
			wantCat:  RejectWrongItem,
			inReason: "does not match",
		},
		{
			name:     "resolution not allowed",
			release:  rel("The.Expanse.S01E01.2160p.WEB-DL.x264-NTb", 50, 2000, 1),
			wantCat:  RejectResolution,
			inReason: "not in the profile's allowed list",
		},
		{
			name:     "resolution absent",
			release:  rel("The.Expanse.S01E01.WEB-DL.x264-NTb", 50, 2000, 1),
			wantCat:  RejectResolution,
			inReason: "does not state a resolution",
		},
		{
			name:     "source not allowed",
			release:  rel("The.Expanse.S01E01.1080p.DVDRip.x264-NTb", 50, 2000, 1),
			wantCat:  RejectSource,
			inReason: "not in the profile's allowed list",
		},
		{
			name:     "below seeder floor",
			release:  rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 1, 2000, 1),
			wantCat:  RejectSeeders,
			inReason: "below the profile floor",
		},
		{
			name:     "too small",
			release:  rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 50, 1),
			wantCat:  RejectSize,
			inReason: "below the",
		},
		{
			name:     "too large",
			release:  rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 90000, 1),
			wantCat:  RejectSize,
			inReason: "exceeds the",
		},
		{
			name:     "banned term",
			release:  rel("The.Expanse.S01E01.1080p.HDCAM.x264-NTb", 50, 2000, 1),
			wantCat:  RejectBannedTerm,
			inReason: "hdcam",
		},
		{
			name:    "missing required term",
			release: rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
			mutate: func(in *Input) {
				in.Profile.RequiredTerms = []string{"atmos"}
			},
			wantCat:  RejectRequiredTerm,
			inReason: "missing the required term",
		},
		{
			name:    "blacklisted hash",
			release: rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
			mutate: func(in *Input) {
				in.Blacklist = map[string]bool{
					hashFor("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb"): true,
				}
			},
			wantCat:  RejectBlacklisted,
			inReason: "blacklisted",
		},
		{
			name:    "not an upgrade, same quality",
			release: rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
			mutate: func(in *Input) {
				in.Imported = &Imported{Resolution: "1080p", Source: "webdl"}
				// No cutoff configured, so the generic same-quality message is
				// the one that should fire.
				in.Profile.UpgradeUntil = ""
			},
			wantCat:  RejectNotAnUpgrade,
			inReason: "same quality",
		},
		{
			// With a cutoff configured, the more specific message wins: there
			// is nothing left to chase, which is different information from
			// "this particular release is no better".
			name:    "not an upgrade, at the profile cutoff",
			release: rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
			mutate: func(in *Input) {
				in.Imported = &Imported{Resolution: "1080p", Source: "webdl"}
				in.Profile.UpgradeUntil = "1080p"
			},
			wantCat:  RejectNotAnUpgrade,
			inReason: "upgrade cutoff",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput([]indexer.Release{tc.release}, wantEp(1, 1))
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			res := Evaluate(in)

			if len(res.Accepted) != 0 {
				t.Fatalf("expected rejection, but the candidate was accepted with score %d",
					res.Accepted[0].Score)
			}
			if len(res.Rejected) != 1 {
				t.Fatalf("got %d rejections, want 1", len(res.Rejected))
			}
			got := res.Rejected[0]
			if got.RejectedBy != tc.wantCat {
				t.Errorf("rejected_by = %q, want %q (reason: %s)", got.RejectedBy, tc.wantCat, got.Reason)
			}
			if !strings.Contains(strings.ToLower(got.Reason), strings.ToLower(tc.inReason)) {
				t.Errorf("reason %q should mention %q", got.Reason, tc.inReason)
			}
			if res.Rejections[tc.wantCat] != 1 {
				t.Errorf("rejection counter for %s = %d, want 1", tc.wantCat, res.Rejections[tc.wantCat])
			}
		})
	}
}

// A cam rip whose specific marker is not in the profile's banned list must
// still be refused, because the parser knows more spellings than any list.
func TestCamIsRejectedEvenWhenNotListed(t *testing.T) {
	in := baseInput([]indexer.Release{
		rel("The.Expanse.S01E01.1080p.TELECINE.x264-NTb", 50, 2000, 1),
	}, wantEp(1, 1))
	in.Profile.BannedTerms = nil

	res := Evaluate(in)
	if len(res.Rejected) != 1 || res.Rejected[0].RejectedBy != RejectBannedTerm {
		t.Fatalf("a telecine should be rejected as a cam-family source, got %+v", res.Rejected)
	}
}

// "ts" is a telesync marker and also a substring of ordinary words. Substring
// matching would reject half of every indexer's catalogue.
func TestBannedTermMatchesWholeWordsOnly(t *testing.T) {
	in := baseInput([]indexer.Release{
		rel("Ghosts.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
	}, &model.Wanted{
		Kind:    model.SubjectEpisode,
		Title:   parser.NormalizeTitle("Ghosts"),
		Season:  1,
		Episode: 1,
	})

	res := Evaluate(in)
	if len(res.Accepted) != 1 {
		t.Fatalf("\"Ghosts\" must not trip the banned term \"ts\": %+v", res.Rejected)
	}
}

// A missing source is tolerated where a missing resolution is not, because
// apibay truncates names at 80 characters and the source is often what falls
// off the end.
func TestMissingSourceIsToleratedMissingResolutionIsNot(t *testing.T) {
	in := baseInput([]indexer.Release{
		rel("The.Expanse.S01E01.1080p.x264-NTb", 50, 2000, 1),
	}, wantEp(1, 1))
	if res := Evaluate(in); len(res.Accepted) != 1 {
		t.Errorf("a release with no source should be acceptable: %+v", res.Rejected)
	}
}

func TestSizeLimitsScaleWithSeasonPackSize(t *testing.T) {
	// 30 GB is far beyond the 12 GB single-episode ceiling, but reasonable for
	// a 13-episode pack.
	pack := rel("The.Expanse.S01.1080p.WEB-DL.x264-NTb", 50, 30000, 100)
	pack.Files = 13

	in := baseInput([]indexer.Release{pack}, wantEp(1, 1, 1, 2, 3))
	res := Evaluate(in)
	if len(res.Accepted) != 1 {
		t.Fatalf("a 30 GB 13-episode pack should pass a 12 GB per-episode ceiling: %+v", res.Rejected)
	}

	// The same bytes as a single episode must be refused.
	single := rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 30000, 100)
	in = baseInput([]indexer.Release{single}, wantEp(1, 1))
	res = Evaluate(in)
	if len(res.Rejected) != 1 || res.Rejected[0].RejectedBy != RejectSize {
		t.Errorf("a 30 GB single episode should be rejected on size: %+v", res)
	}
}

func TestSizeLimitsScaleWithRuntime(t *testing.T) {
	// A 90-minute episode at 20 GB exceeds the flat 12 GB ceiling but fits
	// once the window is scaled by runtime.
	long := rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 20000, 1)

	in := baseInput([]indexer.Release{long}, wantEp(1, 1))
	if res := Evaluate(in); len(res.Rejected) != 1 {
		t.Fatal("without runtime scaling this should exceed the ceiling")
	}

	in.RuntimeMinutes = 90
	if res := Evaluate(in); len(res.Accepted) != 1 {
		t.Errorf("a 90-minute runtime should double the window: %+v", res.Rejected)
	}
}

func TestMovieSizeLimitIsAbsolute(t *testing.T) {
	movie := rel("The.Matrix.1999.1080p.BluRay.x265-GROUP", 50, 20000, 1)
	in := baseInput([]indexer.Release{movie}, &model.Wanted{
		Kind: model.SubjectMovie, Title: parser.NormalizeTitle("The Matrix"), Year: 1999,
	})
	in.RuntimeMinutes = 180

	res := Evaluate(in)
	if len(res.Rejected) != 1 || res.Rejected[0].RejectedBy != RejectSize {
		t.Fatalf("a long movie must not multiply its size ceiling: %+v", res)
	}
	if !strings.Contains(res.Rejected[0].Reason, "12000 MB ceiling") {
		t.Fatalf("unexpected movie size reason: %s", res.Rejected[0].Reason)
	}
}

func TestUpgradeRules(t *testing.T) {
	cases := []struct {
		name     string
		release  string
		imported *Imported
		accept   bool
	}{
		{
			name:     "better resolution is an upgrade",
			release:  "The.Expanse.S01E01.1080p.WEB-DL.x264-NTb",
			imported: &Imported{Resolution: "720p", Source: "webdl"},
			accept:   true,
		},
		{
			name:     "lower resolution is not",
			release:  "The.Expanse.S01E01.720p.WEB-DL.x264-NTb",
			imported: &Imported{Resolution: "1080p", Source: "webdl"},
			accept:   false,
		},
		{
			name:     "better source at the same resolution is an upgrade",
			release:  "The.Expanse.S01E01.1080p.BluRay.x264-NTb",
			imported: &Imported{Resolution: "1080p", Source: "webrip"},
			accept:   true,
		},
		{
			name:     "worse source at the same resolution is not",
			release:  "The.Expanse.S01E01.1080p.HDTV.x264-NTb",
			imported: &Imported{Resolution: "1080p", Source: "bluray"},
			accept:   false,
		},
		{
			name:     "identical quality is not",
			release:  "The.Expanse.S01E01.1080p.WEB-DL.x264-NTb",
			imported: &Imported{Resolution: "1080p", Source: "webdl"},
			accept:   false,
		},
		{
			// The one case worth re-downloading identical quality for.
			name:     "PROPER of the same quality is an upgrade",
			release:  "The.Expanse.S01E01.PROPER.1080p.WEB-DL.x264-NTb",
			imported: &Imported{Resolution: "1080p", Source: "webdl"},
			accept:   true,
		},
		{
			name:     "REPACK of the same quality is an upgrade",
			release:  "The.Expanse.S01E01.REPACK.1080p.WEB-DL.x264-NTb",
			imported: &Imported{Resolution: "1080p", Source: "webdl"},
			accept:   true,
		},
		{
			name:     "a second PROPER is not",
			release:  "The.Expanse.S01E01.PROPER.1080p.WEB-DL.x264-NTb",
			imported: &Imported{Resolution: "1080p", Source: "webdl", Proper: true},
			accept:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput([]indexer.Release{rel(tc.release, 50, 2000, 1)}, wantEp(1, 1))
			in.Imported = tc.imported
			res := Evaluate(in)

			if tc.accept && len(res.Accepted) != 1 {
				t.Errorf("expected an upgrade to be accepted, got %s", res.Rejected[0].Reason)
			}
			if !tc.accept && len(res.Rejected) != 1 {
				t.Errorf("expected rejection, but it was accepted with score %d", res.Accepted[0].Score)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Stage 2: who wins, and why
// ---------------------------------------------------------------------------

func TestWinnerSelection(t *testing.T) {
	cases := []struct {
		name      string
		releases  []indexer.Release
		want      *model.Wanted
		mutate    func(*Input)
		wantTitle string
		because   string
	}{
		{
			name: "resolution beats a huge swarm",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.720p.WEB-DL.x264-NTb", 5000, 1200, 1),
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 40, 2500, 1),
			},
			want:      wantEp(1, 1),
			wantTitle: "1080p",
			because:   "the seeder component is capped so it cannot outweigh a resolution tier",
		},
		{
			name: "source breaks a resolution tie",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.HDTV.x264-NTb", 100, 2000, 1),
				rel("The.Expanse.S01E01.1080p.BluRay.x264-NTb", 100, 2000, 1),
			},
			want:      wantEp(1, 1),
			wantTitle: "BluRay",
			because:   "bluray outranks hdtv in the profile order",
		},
		{
			name: "preferred group breaks a source tie",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NOBODY", 100, 2000, 1),
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 100, 2000, 1),
			},
			want:      wantEp(1, 1),
			wantTitle: "NTb",
			because:   "NTb carries a positive group score",
		},
		{
			name: "a disliked group loses to an unknown one",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-YIFY", 100, 2000, 1),
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-WHOEVER", 100, 2000, 1),
			},
			want:      wantEp(1, 1),
			wantTitle: "WHOEVER",
			because:   "YIFY has a negative profile score",
		},
		{
			name: "PROPER wins an otherwise identical pair",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 100, 2000, 1),
				rel("The.Expanse.S01E01.PROPER.1080p.WEB-DL.x264-NTb", 100, 2000, 1),
			},
			want:      wantEp(1, 1),
			wantTitle: "PROPER",
			because:   "a PROPER is a fix for a broken release",
		},
		{
			name: "an old remux still beats a fresh lesser release",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.720p.WEB-DL.x264-NTb", 100, 1200, 0),
				rel("The.Expanse.S01E01.1080p.BluRay.x264-NTb", 100, 4000, 1095),
			},
			want:      wantEp(1, 1),
			wantTitle: "1080p",
			because:   "the age penalty is capped at 60, so three years cannot outweigh quality",
		},
		{
			name: "season pack wins when several episodes are wanted",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 100, 2000, 1),
				packOf("The.Expanse.S01.1080p.WEB-DL.x264-NTb", 100, 20000, 1, 13),
			},
			want:      wantEp(1, 1, 1, 2, 3, 4, 5),
			wantTitle: "S01.1080p",
			because:   "the pack supplies five wanted episodes, not one",
		},
		{
			name: "single episode wins when only one is wanted",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 100, 2000, 1),
				packOf("The.Expanse.S01.1080p.WEB-DL.x264-NTb", 100, 20000, 1, 13),
			},
			want:      wantEp(1, 1, 1),
			wantTitle: "S01E01",
			because:   "a pack that supplies one wanted episode is penalised for over-fetching",
		},
		{
			// The case that motivated the over-fetch penalty: a six-season
			// pack is a far worse way to get one episode than a single-season
			// pack, even though both technically contain it, and even when the
			// big one has a better source.
			name: "a complete-series pack loses to a single-season pack for one episode",
			releases: []indexer.Release{
				packOf("The.Expanse.S01-S06.1080p.BluRay.x265-NOGRP", 100, 140000, 1, 60),
				packOf("The.Expanse.S01.1080p.WEB-DL.x264-NTb", 100, 4000, 1, 13),
			},
			want:      wantEp(1, 1, 1),
			wantTitle: "S01.1080p",
			because:   "the multi-season penalty is doubled, so 139 GB cannot win the right to deliver one file",
		},
		{
			// The penalty must not fire when the pack is genuinely the right
			// answer, or a fresh series would be fetched an episode at a time.
			name: "a season pack still wins when most of the season is wanted",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.BluRay.x264-NTb", 100, 2000, 1),
				packOf("The.Expanse.S01.1080p.WEB-DL.x264-NTb", 100, 20000, 1, 13),
			},
			want:      wantEp(1, 1, 1, 2, 3, 4, 5, 6, 7, 8),
			wantTitle: "S01.1080p",
			because:   "the pack supplies eight wanted episodes",
		},
		{
			name: "a bounded season pack beats a complete-series pack for a missing season",
			releases: []indexer.Release{
				packOf("The.Expanse.S01-S06.1080p.BluRay.x265-NOGRP", 100, 140000, 1, 60),
				packOf("The.Expanse.S01.1080p.WEB-DL.x264-NTb", 100, 20000, 1, 13),
			},
			want:      wantEp(1, 1, 1, 2, 3, 4, 5, 6, 7, 8),
			wantTitle: "S01.1080p",
			because:   "extra seasons are over-fetching even when the current season is fully wanted",
		},
		{
			name: "HDR preference breaks a tie",
			releases: []indexer.Release{
				rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 100, 2000, 1),
				rel("The.Expanse.S01E01.1080p.WEB-DL.DV.HDR.x264-NTb", 100, 2000, 1),
			},
			want: wantEp(1, 1),
			mutate: func(in *Input) {
				in.Profile.HDRPrefs = []string{"dv", "hdr10"}
			},
			wantTitle: "DV.HDR",
			because:   "the profile asks for Dolby Vision",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(tc.releases, tc.want)
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			res := Evaluate(in)

			best := res.Best()
			if best == nil {
				t.Fatalf("nothing was acceptable: %s", res.Summary())
			}
			if !strings.Contains(best.Release.Title, tc.wantTitle) {
				t.Errorf("winner = %q, want one containing %q\n  because: %s\n  scores: %s",
					best.Release.Title, tc.wantTitle, tc.because, scoreLines(res))
			}
		})
	}
}

func packOf(title string, seeders, sizeMB, ageDays, files int) indexer.Release {
	r := rel(title, seeders, sizeMB, ageDays)
	r.Files = files
	return r
}

func scoreLines(res Result) string {
	var b strings.Builder
	for _, c := range res.Accepted {
		b.WriteString("\n    ")
		b.WriteString(c.Release.Title)
		b.WriteString(" => ")
		b.WriteString(itoa(c.Score))
		for _, comp := range c.Components {
			if comp.Points != 0 {
				b.WriteString("  ")
				b.WriteString(comp.Name)
				b.WriteString("=")
				b.WriteString(itoa(comp.Points))
			}
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// The spec's headline example of a hostile case: swarm size must not buy
// quality.
func TestSeederComponentSaturates(t *testing.T) {
	low := seederComponent(Candidate{Release: indexer.Release{Seeders: 31}}, 150)
	high := seederComponent(Candidate{Release: indexer.Release{Seeders: 5000}}, 150)
	if low.Points != high.Points {
		t.Errorf("31 seeders scored %d and 5000 scored %d; the component should be saturated at both",
			low.Points, high.Points)
	}
	if low.Points != 150 {
		t.Errorf("expected the cap of 150, got %d", low.Points)
	}

	few := seederComponent(Candidate{Release: indexer.Release{Seeders: 4}}, 150)
	if few.Points >= low.Points {
		t.Errorf("4 seeders (%d) should score below 31 seeders (%d)", few.Points, low.Points)
	}
	if zero := seederComponent(Candidate{Release: indexer.Release{Seeders: 0}}, 150); zero.Points != 0 {
		t.Errorf("zero seeders scored %d, want 0", zero.Points)
	}
}

// An absent group must be neutral, not a penalty: two in five releases on this
// indexer have no recoverable group, and penalising them would systematically
// prefer whichever names happened to fit in 80 characters.
func TestTruncatedNameDoesNotPenaliseTheGroup(t *testing.T) {
	in := baseInput(nil, wantEp(1, 1))
	truncated := Candidate{Parsed: parser.Parsed{ReleaseGroup: "", Truncated: true}}
	comp := groupComponent(truncated, in)
	if comp.Points != 0 {
		t.Errorf("a truncated name scored %d for its group; want 0", comp.Points)
	}
	if !strings.Contains(comp.Detail, "truncated") {
		t.Errorf("the detail should explain why the group is unknown, got %q", comp.Detail)
	}
}

func TestGroupMatchIsCaseInsensitive(t *testing.T) {
	in := baseInput(nil, wantEp(1, 1))
	for _, name := range []string{"NTb", "ntb", "NTB"} {
		c := Candidate{Parsed: parser.Parsed{ReleaseGroup: name}}
		if got := groupComponent(c, in); got.Points <= 0 {
			t.Errorf("group %q scored %d; capitalisation should not matter", name, got.Points)
		}
	}
}

func TestAgePenaltyIsCapped(t *testing.T) {
	in := baseInput(nil, wantEp(1, 1))
	old := Candidate{Release: indexer.Release{PublishedAt: testNow.AddDate(-3, 0, 0)}}
	comp := ageComponent(old, in)
	if comp.Points != -60 {
		t.Errorf("a three-year-old release scored %d; the cap of 60 was not applied", comp.Points)
	}
	if !strings.Contains(comp.Detail, "capped") {
		t.Errorf("detail should say the penalty was capped, got %q", comp.Detail)
	}

	// Uncapped, the same release would score -1095 and dominate resolution.
	in.Weights.AgePenaltyMax = 0
	if uncapped := ageComponent(old, in); uncapped.Points > -1000 {
		t.Errorf("with the cap disabled the penalty should be huge, got %d", uncapped.Points)
	}
}

func TestSummaryCountsAndOrders(t *testing.T) {
	releases := []indexer.Release{
		rel("The.Expanse.S01E01.2160p.WEB-DL.x264-NTb", 50, 2000, 1),  // resolution
		rel("The.Expanse.S01E01.2160p.WEB-DL.x265-FLUX", 50, 2000, 1), // resolution
		rel("The.Expanse.S01E01.480p.WEB-DL.x264-NTb", 50, 2000, 1),   // resolution
		rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 1, 2000, 1),   // seeders
		rel("The.Wire.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),     // wrong item
		rel("The.Expanse.S01E01.1080p.BluRay.x264-NTb", 50, 2000, 1),  // accepted
	}
	res := Evaluate(baseInput(releases, wantEp(1, 1)))

	if res.Considered() != 6 {
		t.Errorf("considered %d, want 6", res.Considered())
	}
	if len(res.Accepted) != 1 {
		t.Fatalf("accepted %d, want 1: %s", len(res.Accepted), res.Summary())
	}
	summary := res.Summary()
	if !strings.HasPrefix(summary, "6 candidates, 5 rejected: ") {
		t.Errorf("summary = %q", summary)
	}
	// Largest group first.
	if !strings.Contains(summary, "3 wrong resolution") {
		t.Errorf("summary should lead with the biggest bucket, got %q", summary)
	}
}

func TestEmptyInput(t *testing.T) {
	res := Evaluate(baseInput(nil, wantEp(1, 1)))
	if res.Best() != nil {
		t.Error("Best() on an empty set should be nil")
	}
	if got := res.Summary(); got != "no candidates" {
		t.Errorf("Summary() = %q", got)
	}
}

// A manual browse-style search has no single item to match, and must not
// reject everything for failing to be that item.
func TestNilWantSkipsItemMatching(t *testing.T) {
	releases := []indexer.Release{
		rel("The.Expanse.S01E01.1080p.WEB-DL.x264-NTb", 50, 2000, 1),
		rel("Completely.Different.Show.S03E04.1080p.WEB-DL.x264-FLUX", 50, 2000, 1),
	}
	res := Evaluate(baseInput(releases, nil))
	if len(res.Accepted) != 2 {
		t.Errorf("with no wanted item both should be acceptable, got %s", res.Summary())
	}
}

// Ranking must be stable: the engine and the API score the same set
// independently and have to agree on the winner.
func TestRankingIsDeterministic(t *testing.T) {
	releases := []indexer.Release{
		rel("The.Expanse.S01E01.1080p.WEB-DL.x264-AAA", 100, 2000, 1),
		rel("The.Expanse.S01E01.1080p.WEB-DL.x264-BBB", 100, 2000, 1),
		rel("The.Expanse.S01E01.1080p.WEB-DL.x264-CCC", 100, 2000, 1),
	}
	first := Evaluate(baseInput(releases, wantEp(1, 1)))
	for i := 0; i < 20; i++ {
		again := Evaluate(baseInput(releases, wantEp(1, 1)))
		if again.Best().Release.InfoHash != first.Best().Release.InfoHash {
			t.Fatal("identical inputs produced a different winner")
		}
	}
}

func TestComponentsExplainTheScore(t *testing.T) {
	in := baseInput([]indexer.Release{
		rel("The.Expanse.S01E01.1080p.BluRay.x264-NTb", 100, 2000, 10),
	}, wantEp(1, 1))

	best := Evaluate(in).Best()
	if best == nil {
		t.Fatal("expected an acceptable candidate")
	}

	sum := 0
	names := map[string]bool{}
	for _, comp := range best.Components {
		sum += comp.Points
		names[comp.Name] = true
	}
	if sum != best.Score {
		t.Errorf("components sum to %d but Score is %d", sum, best.Score)
	}
	for _, required := range []string{"resolution", "source", "group", "language", "seeders", "age"} {
		if !names[required] {
			t.Errorf("component %q missing from the breakdown", required)
		}
	}
}

func TestProfileToModelRoundTrip(t *testing.T) {
	cp := config.Profile{
		Name:               "Test",
		Default:            true,
		AllowedResolutions: []string{"1080p", "720p"},
		AllowedSources:     []string{"bluray"},
		MinSizeMB:          100,
		MaxSizeMB:          9000,
		MinSeeders:         3,
		BannedTerms:        []string{"cam"},
		PreferredGroups:    map[string]int{"NTb": 300},
		LanguagePrefs:      []string{"en"},
		UpgradeUntil:       "1080p",
	}
	m := cp.ToModel()

	if m.Name != "Test" || !m.IsDefault || m.MaxSizeMB != 9000 || m.UpgradeUntil != "1080p" {
		t.Errorf("conversion lost fields: %+v", m)
	}
	if m.PreferredGroups["NTb"] != 300 {
		t.Errorf("group scores did not survive: %+v", m.PreferredGroups)
	}

	// The converted profile must not alias the config's slices, or a caller
	// mutating one silently changes the other.
	m.AllowedResolutions[0] = "480p"
	if cp.AllowedResolutions[0] != "1080p" {
		t.Error("ToModel aliased the config slice instead of copying it")
	}
}
