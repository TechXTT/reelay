package parser

import (
	"strings"
	"testing"

	"github.com/TechXTT/reelay/internal/model"
)

func wantEpisode(title string, season, episode int) model.Wanted {
	return model.Wanted{
		Kind:    model.SubjectEpisode,
		Title:   NormalizeTitle(title),
		Season:  season,
		Episode: episode,
	}
}

func wantMovie(title string, year int) model.Wanted {
	return model.Wanted{
		Kind:  model.SubjectMovie,
		Title: NormalizeTitle(title),
		Year:  year,
	}
}

func TestMatchesEpisode(t *testing.T) {
	cases := []struct {
		name    string
		release string
		want    model.Wanted
		ok      bool
		// reasonContains is checked only for rejections, so a future rewording
		// of a message does not silently stop testing the rejection path.
		reasonContains string
	}{
		{
			name:    "exact",
			release: "The.Expanse.S01E01.1080p.BluRay.x264-ROVERS",
			want:    wantEpisode("The Expanse", 1, 1),
			ok:      true,
		},
		{
			name:           "wrong episode",
			release:        "The.Expanse.S01E02.1080p.BluRay.x264-ROVERS",
			want:           wantEpisode("The Expanse", 1, 1),
			ok:             false,
			reasonContains: "wanted S01E01",
		},
		{
			name:           "wrong season",
			release:        "The.Expanse.S02E01.1080p.BluRay.x264-ROVERS",
			want:           wantEpisode("The Expanse", 1, 1),
			ok:             false,
			reasonContains: "wanted S01E01",
		},
		{
			name:           "wrong show",
			release:        "The.Wire.S01E01.1080p.BluRay.x264-ROVERS",
			want:           wantEpisode("The Expanse", 1, 1),
			ok:             false,
			reasonContains: "does not match",
		},
		{
			name:    "article dropped by the indexer",
			release: "Expanse.S01E01.1080p.BluRay.x264-ROVERS",
			want:    wantEpisode("The Expanse", 1, 1),
			ok:      true,
		},
		{
			name:    "season pack satisfies any episode in the season",
			release: "The.Expanse.S01.1080p.BluRay.x265-RARBG",
			want:    wantEpisode("The Expanse", 1, 7),
			ok:      true,
		},
		{
			name:           "season pack for the wrong season",
			release:        "The.Expanse.S02.1080p.BluRay.x265-RARBG",
			want:           wantEpisode("The Expanse", 1, 7),
			ok:             false,
			reasonContains: "season pack is S02",
		},
		{
			name:    "multi-season pack covers the middle",
			release: "The.Expanse.S01-S03.1080p.BluRay.x264",
			want:    wantEpisode("The Expanse", 2, 4),
			ok:      true,
		},
		{
			name:    "multi-episode range covers the unnamed middle episode",
			release: "The.Expanse.S01E01-E03.1080p.WEB-DL.x264",
			want:    wantEpisode("The Expanse", 1, 2),
			ok:      true,
		},
		{
			name:           "movie release cannot satisfy an episode",
			release:        "The.Expanse.2015.1080p.BluRay.x264",
			want:           wantEpisode("The Expanse", 1, 1),
			ok:             false,
			reasonContains: "no season number",
		},
		{
			name:           "unparseable name",
			release:        "..........",
			want:           wantEpisode("The Expanse", 1, 1),
			ok:             false,
			reasonContains: "could not be parsed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Matches(Parse(tc.release), tc.want)
			if got.OK != tc.ok {
				t.Fatalf("Matches(%q) OK = %v, want %v (reason %q)", tc.release, got.OK, tc.ok, got.Reason)
			}
			if !tc.ok && tc.reasonContains != "" && !strings.Contains(got.Reason, tc.reasonContains) {
				t.Errorf("rejection reason %q should contain %q", got.Reason, tc.reasonContains)
			}
		})
	}
}

func TestMatchesMovie(t *testing.T) {
	cases := []struct {
		name    string
		release string
		want    model.Wanted
		ok      bool
	}{
		{"exact", "Oppenheimer.2023.1080p.BluRay.x264-SbR", wantMovie("Oppenheimer", 2023), true},
		{"no year in release", "Oppenheimer.1080p.BluRay.x264-SbR", wantMovie("Oppenheimer", 2023), true},
		{"year off by one is tolerated", "Oppenheimer.2024.1080p.BluRay.x264", wantMovie("Oppenheimer", 2023), true},
		{"year off by two is not", "Oppenheimer.2021.1080p.BluRay.x264", wantMovie("Oppenheimer", 2023), false},
		{"wrong film", "Barbie.2023.1080p.BluRay.x264", wantMovie("Oppenheimer", 2023), false},
		{"title that is a year", "1917.2019.1080p.BluRay.x264-SPARKS", wantMovie("1917", 2019), true},
		{"blade runner sequel", "Blade.Runner.2049.2017.2160p.BluRay.x265", wantMovie("Blade Runner 2049", 2017), true},
		{"original is not the sequel", "Blade.Runner.1982.1080p.BluRay.x264", wantMovie("Blade Runner 2049", 2017), false},
		{"episode release cannot satisfy a movie", "Show.S01E01.1080p.WEB-DL.x264", wantMovie("Show", 2020), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Matches(Parse(tc.release), tc.want)
			if got.OK != tc.ok {
				t.Errorf("Matches(%q) OK = %v, want %v (reason %q)", tc.release, got.OK, tc.ok, got.Reason)
			}
		})
	}
}

func TestMatchesUsesAliases(t *testing.T) {
	want := model.Wanted{
		Kind:    model.SubjectEpisode,
		Title:   NormalizeTitle("Attack on Titan"),
		Aliases: []string{NormalizeTitle("Shingeki no Kyojin"), NormalizeTitle("SnK")},
		Season:  4,
		Episode: 28,
	}
	for _, release := range []string{
		"Attack.on.Titan.S04E28.1080p.WEB.x264-GROUP",
		"Shingeki.no.Kyojin.S04E28.1080p.WEB.x264-GROUP",
	} {
		if got := Matches(Parse(release), want); !got.OK {
			t.Errorf("Matches(%q) rejected: %s", release, got.Reason)
		}
	}
	if got := Matches(Parse("Fullmetal.Alchemist.S04E28.1080p.WEB.x264"), want); got.OK {
		t.Error("an unrelated series matched through the alias list")
	}
}

// The fuzzy fallback has to catch a plausible typo without opening the door to
// short unrelated titles.
func TestMatchesFuzzyTolerance(t *testing.T) {
	long := wantEpisode("Everything Everywhere All at Once", 1, 1)
	if got := Matches(Parse("Everythng.Everywhere.All.at.Once.S01E01.1080p.WEB.x264"), long); !got.OK {
		t.Errorf("a one-character typo in a long title should still match: %s", got.Reason)
	}

	// "Alone" and "Alive" are one edit apart and are different shows.
	short := wantEpisode("Alone", 1, 1)
	if got := Matches(Parse("Alive.S01E01.1080p.WEB.x264"), short); got.OK {
		t.Error("short titles must match exactly; Alive matched Alone")
	}
}

func TestMatchesAnimeAbsolute(t *testing.T) {
	want := model.Wanted{
		Kind:       model.SubjectEpisode,
		Title:      NormalizeTitle("Frieren"),
		Season:     1,
		Episode:    28,
		AbsoluteEp: 28,
		IsAnime:    true,
	}
	if got := Matches(Parse("[SubsPlease] Frieren - 28 (1080p) [ABCD].mkv"), want); !got.OK {
		t.Errorf("absolute numbering should match: %s", got.Reason)
	}
	if got := Matches(Parse("[SubsPlease] Frieren - 29 (1080p) [ABCD].mkv"), want); got.OK {
		t.Error("a different absolute number must not match")
	}
}

func TestWantedEpisodesCovered(t *testing.T) {
	want := model.Wanted{
		Kind:           model.SubjectEpisode,
		Title:          NormalizeTitle("The Expanse"),
		Season:         1,
		Episode:        3,
		WantedEpisodes: []int{3, 4, 5, 9},
	}

	single := Parse("The.Expanse.S01E03.1080p.WEB-DL.x264")
	if got := WantedEpisodesCovered(single, want); got != 1 {
		t.Errorf("single episode covers %d, want 1", got)
	}

	pack := Parse("The.Expanse.S01.1080p.BluRay.x265-RARBG")
	if got := WantedEpisodesCovered(pack, want); got != 4 {
		t.Errorf("season pack covers %d, want 4", got)
	}

	multi := Parse("The.Expanse.S01E03-E05.1080p.WEB-DL.x264")
	if got := WantedEpisodesCovered(multi, want); got != 3 {
		t.Errorf("multi-episode covers %d, want 3", got)
	}

	other := Parse("The.Expanse.S02.1080p.BluRay.x265")
	if got := WantedEpisodesCovered(other, want); got != 0 {
		t.Errorf("wrong season covers %d, want 0", got)
	}
}

// Every rejection must carry a reason. The UI promises "11 rejected, and here
// is why"; an empty string breaks that promise.
func TestEveryRejectionHasAReason(t *testing.T) {
	c := loadCorpus(t)
	want := wantEpisode("The Expanse", 1, 1)
	for _, tc := range c.Cases {
		res := Matches(Parse(tc.Name), want)
		if !res.OK && strings.TrimSpace(res.Reason) == "" {
			t.Errorf("Matches(%q) rejected with an empty reason", tc.Name)
		}
	}
}
