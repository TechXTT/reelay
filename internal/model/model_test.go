package model

import (
	"testing"
	"time"
)

// The state machine is the thing that stops an item stalling somewhere no loop
// handles, so its edges are worth asserting explicitly rather than trusting.
func TestStateTransitions(t *testing.T) {
	legal := []struct{ from, to ItemState }{
		{StateUnmonitored, StateWanted},
		{StateWanted, StateSearching},
		{StateSearching, StateGrabbed},
		{StateSearching, StateWanted},
		{StateGrabbed, StateDownloading},
		{StateDownloading, StateImporting},
		{StateImporting, StateImported},
		{StateImporting, StateImportFailed},
		{StateImportFailed, StateWanted},
		{StateImportFailed, StateFailed},
		{StateFailed, StateWanted},
		{StateImported, StateWanted}, // an upgrade search
		{StateWanted, StateWanted},   // idempotent re-assertion
	}
	for _, tc := range legal {
		if !tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s should be legal", tc.from, tc.to)
		}
	}

	illegal := []struct{ from, to ItemState }{
		{StateUnmonitored, StateGrabbed},
		{StateWanted, StateImported},
		{StateWanted, StateDownloading},
		{StateSearching, StateImporting},
		{StateGrabbed, StateImported},
		{StateImported, StateDownloading},
		{StateImporting, StateWanted},
		{StateFailed, StateGrabbed},
		{StateWanted, ItemState("nonsense")},
	}
	for _, tc := range illegal {
		if tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s should be rejected", tc.from, tc.to)
		}
	}
}

func TestStateClassification(t *testing.T) {
	for _, s := range []ItemState{StateGrabbed, StateDownloading, StateImporting} {
		if !s.Active() {
			t.Errorf("%s should be active so the status loop watches it", s)
		}
	}
	for _, s := range []ItemState{StateWanted, StateImported, StateFailed, StateUnmonitored} {
		if s.Active() {
			t.Errorf("%s should not be active", s)
		}
	}
	for _, s := range []ItemState{StateImported, StateFailed, StateUnmonitored} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	if StateWanted.Terminal() {
		t.Error("wanted is not terminal")
	}
}

func TestAllItemStatesAreValid(t *testing.T) {
	for _, s := range AllItemStates {
		if !s.Valid() {
			t.Errorf("%s is in AllItemStates but reports invalid", s)
		}
	}
	if ItemState("almost_imported").Valid() {
		t.Error("an unknown state must not validate")
	}
	// Every state must be reachable from some other state, or it is dead code
	// that an item can never enter.
	for _, target := range AllItemStates {
		reachable := false
		for _, from := range AllItemStates {
			if from != target && from.CanTransitionTo(target) {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("no state can transition into %s", target)
		}
	}
}

func TestEpisodeAired(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	grace := time.Hour

	past := now.Add(-48 * time.Hour)
	if !(Episode{AirDate: &past}).Aired(now, grace) {
		t.Error("an episode that aired two days ago should be aired")
	}

	future := now.Add(48 * time.Hour)
	if (Episode{AirDate: &future}).Aired(now, grace) {
		t.Error("an unaired episode must never be searched for")
	}

	// Inside the grace window: broadcast has happened but releases have not
	// appeared yet, so searching only burns indexer budget.
	justNow := now.Add(-30 * time.Minute)
	if (Episode{AirDate: &justNow}).Aired(now, grace) {
		t.Error("an episode inside the grace window is not yet searchable")
	}

	// A missing air date must not park the episode forever.
	if !(Episode{}).Aired(now, grace) {
		t.Error("an episode with no air date should be treated as aired")
	}
}

func TestProfileRanks(t *testing.T) {
	p := QualityProfile{
		AllowedResolutions: []string{"2160p", "1080p", "720p"},
		AllowedSources:     []string{"remux", "bluray", "webdl"},
	}
	if p.ResolutionRank("2160p") <= p.ResolutionRank("1080p") {
		t.Error("the first listed resolution must rank highest")
	}
	if p.ResolutionRank("480p") != -1 {
		t.Error("a disallowed resolution must rank -1")
	}
	if p.SourceRank("remux") <= p.SourceRank("webdl") {
		t.Error("the first listed source must rank highest")
	}
	if p.SourceRank("hdtv") != -1 {
		t.Error("a disallowed source must rank -1")
	}
}

func TestGrabStateActive(t *testing.T) {
	for _, s := range []GrabState{GrabPending, GrabDownloading, GrabCompleted, GrabImporting} {
		if !s.Active() {
			t.Errorf("%s should be active", s)
		}
	}
	for _, s := range []GrabState{GrabImported, GrabStalled, GrabRemoved, GrabFailed} {
		if s.Active() {
			t.Errorf("%s should not be active", s)
		}
	}
}

func TestEnumValidation(t *testing.T) {
	if !MonitorFutureOnly.Valid() || MonitorMode("sometimes").Valid() {
		t.Error("MonitorMode validation is wrong")
	}
	if !SeriesFollowing.Valid() || SeriesStatus("watching").Valid() {
		t.Error("SeriesStatus validation is wrong")
	}
}

func TestEpisodeString(t *testing.T) {
	e := Episode{Season: 1, Number: 5}
	if got := e.String(); got != "S01E05" {
		t.Errorf("String() = %q, want S01E05", got)
	}
	e = Episode{Season: 12, Number: 134}
	if got := e.String(); got != "S12E134" {
		t.Errorf("String() = %q, want S12E134", got)
	}
}
