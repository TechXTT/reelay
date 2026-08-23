package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/indexer/tpb"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/scoring"
)

// buildIndexers constructs one client per enabled indexer.
//
// This is the only place concrete indexer types are named; everything
// downstream sees indexer.Indexer.
func buildIndexers(cfg *config.Config, log *slog.Logger, clk clock.Clock) ([]indexer.Indexer, error) {
	var out []indexer.Indexer
	for _, ix := range cfg.EnabledIndexers() {
		switch ix.Type {
		case "piratebay":
			c, err := tpb.New(ix, tpb.Options{Clock: clk, Logger: log})
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		default:
			// Config validation already rejects unknown types; reaching here
			// means the two lists drifted apart.
			return nil, fmt.Errorf("indexer %q has unsupported type %q", ix.Name, ix.Type)
		}
	}
	return out, nil
}

// runSearch implements --search: a one-shot live query across every healthy
// indexer, parsed and printed. Nothing is persisted and nothing is grabbed.
//
// This exists to make the indexer and parser inspectable without a database, a
// download client or a browser, which is exactly what you want when a release
// you expected to be found was not.
func runSearch(ctx context.Context, cfg *config.Config, log *slog.Logger, term string, recent bool) error {
	indexers, err := buildIndexers(cfg, log, clock.Real{})
	if err != nil {
		return err
	}
	if len(indexers) == 0 {
		return errors.New("no enabled indexers; check the indexers section of your config")
	}

	q := indexer.Query{Term: term, Recent: recent}
	if recent {
		// A recent listing has no search term to constrain it and otherwise
		// returns music, games and porn alongside video.
		q.Categories = indexer.VideoCategories()
	}

	var collected []indexer.Release
	var problems []string

	for _, ix := range indexers {
		start := time.Now()
		releases, err := ix.Search(ctx, q)
		elapsed := time.Since(start).Round(time.Millisecond)

		switch {
		case errors.Is(err, indexer.ErrNoResults):
			// Worth spelling out, because this is the response that also means
			// "you are being rate limited" and it is the single most likely
			// reason a search comes back empty when it should not.
			problems = append(problems, fmt.Sprintf(
				"%s: returned its no-results marker after %s.\n"+
					"    This means EITHER nothing matched OR the indexer is throttling you —\n"+
					"    the API uses the same response for both. If you have run several\n"+
					"    searches in the last minute, wait and try again before believing it.",
				ix.Name(), elapsed))
			continue
		case errors.Is(err, indexer.ErrUnhealthy):
			problems = append(problems, fmt.Sprintf("%s: skipped, circuit breaker is open (%v)", ix.Name(), err))
			continue
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", ix.Name(), err))
			continue
		}

		fmt.Fprintf(os.Stdout, "%s: %d releases in %s\n", ix.Name(), len(releases), elapsed)
		collected = append(collected, releases...)
	}

	for _, p := range problems {
		fmt.Fprintf(os.Stdout, "%s\n", p)
	}
	if len(collected) == 0 {
		fmt.Fprintln(os.Stdout, "\nNo releases to show.")
		return nil
	}

	profile := cfg.DefaultProfile()
	if profile == nil {
		return errors.New("no quality profiles configured")
	}
	want := wantFromTerm(term, recent)

	res := scoring.Evaluate(scoring.Input{
		Releases: collected,
		Want:     want,
		Profile:  profile.ToModel(),
		Weights:  cfg.Scoring,
		Now:      time.Now(),
	})

	fmt.Fprintf(os.Stdout, "\nprofile %q: %s\n", profile.Name, res.Summary())
	if want == nil {
		fmt.Fprintf(os.Stdout,
			"the search term does not identify a specific episode or movie, so the\n"+
				"item-match filter is off and everything is scored side by side\n")
	} else {
		fmt.Fprintf(os.Stdout, "matching against: %s\n", describeWant(*want))
	}

	printAccepted(res)
	printRejected(res)
	printParseSummary(collected)
	return nil
}

// wantFromTerm derives the matching target from the search term by parsing the
// term itself, so `--search "the expanse s01e01"` evaluates exactly what the
// engine would for that episode.
//
// Returns nil when the term names no specific item — "the expanse" alone is a
// browse, not a request for one episode — which switches the item-match filter
// off rather than rejecting every candidate for not being a movie.
func wantFromTerm(term string, recent bool) *model.Wanted {
	if recent || strings.TrimSpace(term) == "" {
		return nil
	}
	p := parser.Parse(term)
	if p.Title == "" {
		return nil
	}

	switch {
	case p.Season > 0 && len(p.Episodes) > 0:
		return &model.Wanted{
			Kind:           model.SubjectEpisode,
			Title:          p.Title,
			Season:         p.Season,
			Episode:        p.Episodes[0],
			AbsoluteEp:     p.AbsoluteEp,
			WantedEpisodes: []int{p.Episodes[0]},
		}
	case p.Year > 0 && !p.HasEpisodeInfo():
		return &model.Wanted{
			Kind:  model.SubjectMovie,
			Title: p.Title,
			Year:  p.Year,
		}
	default:
		// A bare title, or a season with no episode: nothing specific enough
		// to match against.
		return nil
	}
}

func describeWant(w model.Wanted) string {
	if w.Kind == model.SubjectMovie {
		return fmt.Sprintf("movie %q (%d)", w.Title, w.Year)
	}
	return fmt.Sprintf("episode %q S%02dE%02d", w.Title, w.Season, w.Episode)
}

func printAccepted(res scoring.Result) {
	if len(res.Accepted) == 0 {
		fmt.Fprintln(os.Stdout, "\nnothing acceptable — see the rejections below")
		return
	}

	fmt.Fprintf(os.Stdout, "\n=== %d acceptable, best first ===\n", len(res.Accepted))
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SCORE\tSEED\tSIZE\tTITLE\tSEASON/EP\tRES\tSOURCE\tCODEC\tGROUP\tAGE")
	fmt.Fprintln(w, "-----\t----\t----\t-----\t---------\t---\t------\t-----\t-----\t---")
	for i, c := range res.Accepted {
		if i >= 20 {
			fmt.Fprintf(w, "...\t\t\t(%d more)\t\t\t\t\t\t\n", len(res.Accepted)-i)
			break
		}
		fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			c.Score,
			c.Release.Seeders,
			indexer.HumanSize(c.Release.SizeBytes),
			truncate(c.Parsed.Title, 30),
			numbering(c.Parsed),
			orDash(c.Parsed.Resolution),
			orDash(c.Parsed.Source),
			orDash(c.Parsed.VideoCodec),
			orDash(c.Parsed.ReleaseGroup),
			age(c.Release.PublishedAt),
		)
	}
	_ = w.Flush()

	// Spell out the winner's arithmetic. "Why did it pick that one" should not
	// require reading the source.
	best := res.Accepted[0]
	fmt.Fprintf(os.Stdout, "\nwinner: %s\n", best.Release.Title)
	for _, comp := range best.Components {
		sign := "+"
		if comp.Points < 0 {
			sign = ""
		}
		detail := ""
		if comp.Detail != "" {
			detail = "  " + comp.Detail
		}
		fmt.Fprintf(os.Stdout, "  %s%-5d %-14s%s\n", sign, comp.Points, comp.Name, detail)
	}
	fmt.Fprintf(os.Stdout, "  =%-5d total\n", best.Score)
}

func printRejected(res scoring.Result) {
	if len(res.Rejected) == 0 {
		return
	}
	fmt.Fprintf(os.Stdout, "\n=== %d rejected ===\n", len(res.Rejected))

	// One worked example per category is enough to diagnose a filter; the
	// counts carry the rest.
	shown := map[string]int{}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "REASON\tCOUNT\tEXAMPLE\tWHY")
	fmt.Fprintln(w, "------\t-----\t-------\t---")
	for _, c := range res.Rejected {
		if shown[c.RejectedBy] > 0 {
			continue
		}
		shown[c.RejectedBy]++
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n",
			c.RejectedBy,
			res.Rejections[c.RejectedBy],
			truncate(c.Parsed.Title, 24),
			truncate(c.Reason, 62))
	}
	_ = w.Flush()
}

// printParseSummary answers the question that actually matters when a search
// disappoints: did the indexer not have it, or did we fail to understand what
// it sent?
func printParseSummary(releases []indexer.Release) {
	var noTitle, noGroup, truncated, tv, movies int
	for _, r := range releases {
		p := parser.Parse(r.Title)
		if p.Title == "" {
			noTitle++
		}
		if p.ReleaseGroup == "" {
			noGroup++
		}
		if p.Truncated {
			truncated++
		}
		if p.HasEpisodeInfo() {
			tv++
		} else {
			movies++
		}
	}
	fmt.Fprintf(os.Stdout,
		"\nparsed: %d TV, %d movie-shaped, %d unparseable titles\n"+
			"        %d without a release group, %d hit the indexer's 80-char name limit\n",
		tv, movies, noTitle, noGroup, truncated)
	if noGroup > len(releases)/4 {
		fmt.Fprintf(os.Stdout,
			"        (a high no-group count is normal for this indexer: it truncates\n"+
				"         names and appends the uploader, so preferred_groups scoring is\n"+
				"         inactive for those candidates)\n")
	}
}

// numbering renders the episode identity compactly.
func numbering(p parser.Parsed) string {
	switch {
	case p.AirDate != "":
		return p.AirDate
	case p.SeasonEnd > 0:
		return fmt.Sprintf("S%02d-S%02d", p.Season, p.SeasonEnd)
	case p.IsSeasonPack && len(p.Episodes) == 0:
		return fmt.Sprintf("S%02d pack", p.Season)
	case len(p.Episodes) > 4:
		return fmt.Sprintf("S%02dE%02d-E%02d", p.Season, p.Episodes[0], p.Episodes[len(p.Episodes)-1])
	case len(p.Episodes) > 0:
		var b strings.Builder
		fmt.Fprintf(&b, "S%02d", p.Season)
		for _, e := range p.Episodes {
			fmt.Fprintf(&b, "E%02d", e)
		}
		return b.String()
	case p.AbsoluteEp > 0:
		return fmt.Sprintf("#%d", p.AbsoluteEp)
	case p.Year > 0:
		return fmt.Sprintf("(%d)", p.Year)
	default:
		return "-"
	}
}

func truncate(s string, n int) string {
	if s == "" {
		return "-"
	}
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
