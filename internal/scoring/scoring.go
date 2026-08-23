// Package scoring decides which candidate release to grab.
//
// Two stages, kept strictly separate, because they answer different questions
// and conflating them is how you end up unable to explain a decision:
//
//	Stage 1 — hard filters. Is this release ACCEPTABLE at all? A candidate that
//	          fails any filter is discarded with a recorded reason, so the UI
//	          can say "12 candidates, 11 rejected: 8 wrong resolution, 3 below
//	          the seeder floor" instead of showing nothing.
//
//	Stage 2 — ranking. Among the acceptable ones, which is BEST? A weighted sum
//	          with a per-component breakdown, so a surprising winner can be
//	          explained rather than argued about.
//
// Nothing here does I/O or touches the database. The whole package is a pure
// function of (releases, profile, weights, what we already have).
package scoring

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
)

// Rejection categories. Stable strings: the API groups by them and the UI
// renders the counts, so they are part of the contract.
const (
	RejectUnparseable  = "unparseable"
	RejectWrongItem    = "wrong_item"
	RejectResolution   = "resolution"
	RejectSource       = "source"
	RejectSize         = "size"
	RejectSeeders      = "seeders"
	RejectBannedTerm   = "banned_term"
	RejectRequiredTerm = "required_term"
	RejectBlacklisted  = "blacklisted"
	RejectNotAnUpgrade = "not_an_upgrade"
)

// rejectionLabels render a category for humans, in the summary line.
var rejectionLabels = map[string]string{
	RejectUnparseable:  "unparseable name",
	RejectWrongItem:    "wrong item",
	RejectResolution:   "wrong resolution",
	RejectSource:       "wrong source",
	RejectSize:         "size out of range",
	RejectSeeders:      "below seeder floor",
	RejectBannedTerm:   "banned term",
	RejectRequiredTerm: "missing required term",
	RejectBlacklisted:  "blacklisted for this item",
	RejectNotAnUpgrade: "not an upgrade",
}

// Imported describes the file already on disk, so an upgrade can be
// distinguished from a sideways move.
type Imported struct {
	Resolution string
	Source     string
	Proper     bool
	Repack     bool
}

// Input is everything a decision needs.
type Input struct {
	Releases []indexer.Release

	// Want is the specific episode or movie being searched for. Nil skips the
	// item-match filter entirely, which is what a browse-style manual search
	// needs — there is no single item to match against.
	Want *model.Wanted

	Profile model.QualityProfile
	Weights config.Scoring

	// Now anchors the age penalty. Injected rather than read from the clock so
	// scoring stays a pure function and its tests need no fake clock.
	Now time.Time

	// Blacklist holds info hashes already tried and failed for this item.
	Blacklist map[string]bool

	// Imported is nil when nothing has been imported yet.
	Imported *Imported

	// RuntimeMinutes scales the size limits. Zero means "assume a normal
	// 45-minute episode".
	RuntimeMinutes int
}

// Component is one contribution to a candidate's score, kept so the API can
// show why a release won.
type Component struct {
	Name   string `json:"name"`
	Points int    `json:"points"`
	Detail string `json:"detail,omitempty"`
}

// Candidate is a release plus everything we concluded about it.
type Candidate struct {
	Release indexer.Release `json:"release"`
	Parsed  parser.Parsed   `json:"parsed"`

	Score      int         `json:"score"`
	Components []Component `json:"components,omitempty"`

	// RejectedBy is "" for accepted candidates.
	RejectedBy string `json:"rejected_by,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (c Candidate) Accepted() bool { return c.RejectedBy == "" }

// Result is the outcome of evaluating a candidate set.
type Result struct {
	// Accepted is ranked best first.
	Accepted []Candidate `json:"accepted"`
	Rejected []Candidate `json:"rejected"`
	// Rejections counts rejections per category.
	Rejections map[string]int `json:"rejections,omitempty"`
}

// Best returns the winner, or nil when nothing was acceptable.
func (r Result) Best() *Candidate {
	if len(r.Accepted) == 0 {
		return nil
	}
	return &r.Accepted[0]
}

// Considered is the total number of candidates evaluated.
func (r Result) Considered() int { return len(r.Accepted) + len(r.Rejected) }

// Summary is the one-line explanation the spec asks the UI to show.
func (r Result) Summary() string {
	total := r.Considered()
	if total == 0 {
		return "no candidates"
	}
	if len(r.Rejected) == 0 {
		return fmt.Sprintf("%d candidates, all acceptable", total)
	}

	// Highest count first, then alphabetical, so the line is stable between
	// runs and diffable in logs.
	type kv struct {
		cat string
		n   int
	}
	pairs := make([]kv, 0, len(r.Rejections))
	for cat, n := range r.Rejections {
		pairs = append(pairs, kv{cat, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].cat < pairs[j].cat
	})

	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		label := rejectionLabels[p.cat]
		if label == "" {
			label = p.cat
		}
		parts = append(parts, fmt.Sprintf("%d %s", p.n, label))
	}
	return fmt.Sprintf("%d candidates, %d rejected: %s",
		total, len(r.Rejected), strings.Join(parts, ", "))
}

// Evaluate runs both stages and returns the full ranked list plus every
// rejection.
//
// The rejected candidates are returned, not discarded: "why didn't it grab X"
// is unanswerable without them, and the API exposes the list so a human can
// override the decision.
func Evaluate(in Input) Result {
	res := Result{
		Accepted:   make([]Candidate, 0, len(in.Releases)),
		Rejected:   make([]Candidate, 0),
		Rejections: map[string]int{},
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}

	for _, rel := range in.Releases {
		c := Candidate{Release: rel, Parsed: parser.Parse(rel.Title)}

		if cat, reason := reject(&c, in); cat != "" {
			c.RejectedBy = cat
			c.Reason = reason
			res.Rejected = append(res.Rejected, c)
			res.Rejections[cat]++
			continue
		}

		c.Components = componentsFor(c, in)
		for _, comp := range c.Components {
			c.Score += comp.Points
		}
		res.Accepted = append(res.Accepted, c)
	}

	sort.SliceStable(res.Accepted, func(i, j int) bool {
		a, b := res.Accepted[i], res.Accepted[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		// Deterministic tie-breaks: healthier swarm first, then the info hash
		// so the same candidate set always produces the same winner.
		if a.Release.Seeders != b.Release.Seeders {
			return a.Release.Seeders > b.Release.Seeders
		}
		return a.Release.InfoHash < b.Release.InfoHash
	})

	// Rejected candidates are ordered for display, not by merit.
	sort.SliceStable(res.Rejected, func(i, j int) bool {
		if res.Rejected[i].RejectedBy != res.Rejected[j].RejectedBy {
			return res.Rejected[i].RejectedBy < res.Rejected[j].RejectedBy
		}
		return res.Rejected[i].Release.Seeders > res.Rejected[j].Release.Seeders
	})

	return res
}
