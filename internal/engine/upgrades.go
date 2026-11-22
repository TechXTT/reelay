package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/store"
)

// upgradeBatch bounds how many imported movies and, separately, episodes one
// upgrade pass examines.
const upgradeBatch = 200

// upgradeListLimit bounds how many imported movies and, separately, episodes
// the upgrades listing scans.
const upgradeListLimit = 2000

var (
	ErrUpgradesDisabled = errors.New("upgrades are disabled")
	ErrUpgradeLimit     = errors.New("upgrade limit reached")
)

// NotUpgradableError says why an item cannot start an upgrade search now.
type NotUpgradableError struct{ Reason string }

func (e *NotUpgradableError) Error() string { return e.Reason }

// UpgradeItem is one imported item whose profile sets an upgrade cutoff above
// its imported resolution.
type UpgradeItem struct {
	SubjectType    model.SubjectType `json:"subject_type"`
	ID             int64             `json:"id"`
	Title          string            `json:"title"`
	Quality        string            `json:"quality"`
	Cutoff         string            `json:"cutoff"`
	Profile        string            `json:"profile"`
	UpgradeChecked *time.Time        `json:"upgrade_checked_at,omitempty"`
	State          model.ItemState   `json:"state"`
	Upgrading      bool              `json:"upgrading"`
}

// UpgradeStatus is the upgrades listing plus the settings that govern it.
// Truncated is set when the scan bound was reached and more items may exist.
type UpgradeStatus struct {
	Enabled   bool
	MaxGrabs  int
	Inflight  int
	Truncated bool
	Items     []UpgradeItem
}

// upgradeSubject is an imported item with the profile that judges it.
type upgradeSubject struct {
	subject   model.SubjectType
	id        int64
	title     string
	state     model.ItemState
	quality   string
	checkedAt *time.Time
	profile   model.QualityProfile
}

// belowCutoff reports whether the imported resolution ranks below the
// profile's upgrade cutoff. Unparseable imported quality never qualifies.
func (u upgradeSubject) belowCutoff() bool {
	var imported = importedQuality(u.quality)

	if imported == nil || u.profile.UpgradeUntil == "" {
		return false
	}
	return u.profile.ResolutionRank(imported.Resolution) < u.profile.ResolutionRank(u.profile.UpgradeUntil)
}

// detail names the quality gap for the audit trail.
func (u upgradeSubject) detail() string {
	var imported = importedQuality(u.quality)

	return fmt.Sprintf("imported %s %s below cutoff %s", imported.Resolution, imported.Source, u.profile.UpgradeUntil)
}

// upgradeResolver loads profiles and series once per pass.
type upgradeResolver struct {
	e        *Engine
	profiles map[int64]model.QualityProfile
	series   map[int64]model.Series
}

func (e *Engine) newUpgradeResolver() *upgradeResolver {
	return &upgradeResolver{e: e, profiles: map[int64]model.QualityProfile{}, series: map[int64]model.Series{}}
}

func (r *upgradeResolver) profile(ctx context.Context, id int64) (model.QualityProfile, error) {
	if p, ok := r.profiles[id]; ok {
		return p, nil
	}
	p, err := r.e.store.Profiles().Get(ctx, id)
	if err == nil {
		r.profiles[id] = p
	}
	return p, err
}

func (r *upgradeResolver) movie(ctx context.Context, m model.Movie) (upgradeSubject, error) {
	p, err := r.profile(ctx, m.ProfileID)
	if err != nil {
		return upgradeSubject{}, err
	}
	return upgradeSubject{subject: model.SubjectMovie, id: m.ID, title: m.Title, state: m.State,
		quality: m.ImportedQuality, checkedAt: m.UpgradeCheckedAt, profile: p}, nil
}

func (r *upgradeResolver) episode(ctx context.Context, ep model.Episode) (upgradeSubject, error) {
	series, ok := r.series[ep.SeriesID]
	if !ok {
		var err error

		series, err = r.e.store.Series().Get(ctx, ep.SeriesID)
		if err != nil {
			return upgradeSubject{}, err
		}
		r.series[ep.SeriesID] = series
	}
	p, err := r.profile(ctx, series.ProfileID)
	if err != nil {
		return upgradeSubject{}, err
	}
	return upgradeSubject{subject: model.SubjectEpisode, id: ep.ID, title: series.Title + " " + ep.String(),
		state: ep.State, quality: ep.ImportedQuality, checkedAt: ep.UpgradeCheckedAt, profile: p}, nil
}

func (r *upgradeResolver) byID(ctx context.Context, subject model.SubjectType, id int64) (upgradeSubject, error) {
	if subject == model.SubjectMovie {
		movie, err := r.e.store.Movies().Get(ctx, id)
		if err != nil {
			return upgradeSubject{}, err
		}
		return r.movie(ctx, movie)
	}
	episode, err := r.e.store.Episodes().Get(ctx, id)
	if err != nil {
		return upgradeSubject{}, err
	}
	return r.episode(ctx, episode)
}

func (e *Engine) upgradeInflight(ctx context.Context) (int, error) {
	movies, err := e.store.Movies().InflightUpgrades(ctx)
	if err != nil {
		return 0, err
	}
	episodes, err := e.store.Episodes().InflightUpgrades(ctx)
	if err != nil {
		return 0, err
	}
	return movies + episodes, nil
}

// startUpgrade moves an imported item back to wanted, due now.
func (e *Engine) startUpgrade(ctx context.Context, u upgradeSubject) error {
	if err := e.store.Transitions().StartUpgrade(ctx, u.subject, u.id, "upgrade_search", u.detail()); err != nil {
		return err
	}
	e.publish("state_transition", u.subject, u.id, map[string]any{"state": model.StateWanted, "reason": "upgrade_search"})
	return nil
}

// upgradeSkippable reports a start failure that only means the item changed or
// is busy, so the pass leaves it unchecked and moves on.
func upgradeSkippable(err error) bool {
	return errors.Is(err, store.ErrItemBusy) || errors.Is(err, store.ErrLocked) ||
		errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrInvalidTransition)
}

// UpgradeOnce promotes imported items below their profile's upgrade cutoff to
// wanted, keeping at most max_upgrade_grabs upgrades in flight and checking
// each item once per upgrade_interval. The search loop does the searching, and
// a search that finds nothing returns the item to imported.
func (e *Engine) UpgradeOnce(ctx context.Context) error {
	var now = e.clock.Now().UTC()
	var resolver = e.newUpgradeResolver()
	var subjects []upgradeSubject
	var checkedMovies, checkedEpisodes []int64
	var errs []error
	var promoted, waiting int

	e.upgradeMu.Lock()
	defer e.upgradeMu.Unlock()
	inflight, err := e.upgradeInflight(ctx)
	if err != nil {
		return err
	}
	free := e.cfg.Downloader.MaxUpgradeGrabs - inflight
	if free <= 0 {
		return nil
	}
	dueBefore := now.Add(-e.cfg.Schedules.UpgradeInterval.Duration)
	movies, err := e.store.Movies().UpgradeDue(ctx, dueBefore, upgradeBatch)
	if err != nil {
		return err
	}
	episodes, err := e.store.Episodes().UpgradeDue(ctx, dueBefore, upgradeBatch)
	if err != nil {
		return err
	}
	for _, movie := range movies {
		u, err := resolver.movie(ctx, movie)
		if err != nil {
			return err
		}
		subjects = append(subjects, u)
	}
	for _, episode := range episodes {
		u, err := resolver.episode(ctx, episode)
		if err != nil {
			return err
		}
		subjects = append(subjects, u)
	}
	// Never-checked items first, then the longest unchecked.
	slices.SortStableFunc(subjects, func(a, b upgradeSubject) int {
		switch {
		case a.checkedAt == nil && b.checkedAt == nil:
			return 0
		case a.checkedAt == nil:
			return -1
		case b.checkedAt == nil:
			return 1
		}
		return a.checkedAt.Compare(*b.checkedAt)
	})
	for _, u := range subjects {
		if u.belowCutoff() {
			if promoted >= free {
				// Over the cap: keep the old check time so it goes first next pass.
				waiting++
				continue
			}
			if err := e.startUpgrade(ctx, u); err != nil {
				if !upgradeSkippable(err) {
					errs = append(errs, err)
				}
				continue
			}
			promoted++
			continue
		}
		if u.subject == model.SubjectMovie {
			checkedMovies = append(checkedMovies, u.id)
		} else {
			checkedEpisodes = append(checkedEpisodes, u.id)
		}
	}
	// Promoted items were stamped when they started; stamp the rest now.
	if err := e.store.Movies().MarkUpgradeChecked(ctx, checkedMovies, now); err != nil {
		errs = append(errs, err)
	}
	if err := e.store.Episodes().MarkUpgradeChecked(ctx, checkedEpisodes, now); err != nil {
		errs = append(errs, err)
	}
	if promoted > 0 {
		e.log.Info("upgrade searches started", "started", promoted, "waiting_for_slot", waiting,
			"not_below_cutoff", len(checkedMovies)+len(checkedEpisodes), "in_flight", inflight+promoted)
		_ = e.Trigger("search")
	}
	return errors.Join(errs...)
}

// StartUpgrade starts an upgrade search for one imported item below its
// profile's cutoff, as UpgradeOnce would. It returns ErrUpgradesDisabled,
// *NotUpgradableError or ErrUpgradeLimit when the item cannot start now, and
// store.ErrNotFound for an unknown item.
func (e *Engine) StartUpgrade(ctx context.Context, subject model.SubjectType, id int64) error {
	if e.cfg.Schedules.UpgradeInterval.Duration <= 0 {
		return ErrUpgradesDisabled
	}
	u, err := e.newUpgradeResolver().byID(ctx, subject, id)
	if err != nil {
		return err
	}
	switch {
	case u.state != model.StateImported:
		return &NotUpgradableError{Reason: fmt.Sprintf("item is %s, not imported", strings.ReplaceAll(string(u.state), "_", " "))}
	case u.profile.UpgradeUntil == "":
		return &NotUpgradableError{Reason: "the quality profile has no upgrade cutoff"}
	case importedQuality(u.quality) == nil:
		return &NotUpgradableError{Reason: "the imported quality is unknown"}
	case !u.belowCutoff():
		return &NotUpgradableError{Reason: fmt.Sprintf("already at or above the %s cutoff", u.profile.UpgradeUntil)}
	}
	e.upgradeMu.Lock()
	defer e.upgradeMu.Unlock()
	inflight, err := e.upgradeInflight(ctx)
	if err != nil {
		return err
	}
	if inflight >= e.cfg.Downloader.MaxUpgradeGrabs {
		return ErrUpgradeLimit
	}
	if err := e.startUpgrade(ctx, u); err != nil {
		if upgradeSkippable(err) {
			return &NotUpgradableError{Reason: "the item is busy"}
		}
		return err
	}
	return e.Trigger("search")
}

// Upgrades lists imported items, including those mid-upgrade, whose profile
// cutoff is above their imported resolution: upgrading ones first, then by
// title.
func (e *Engine) Upgrades(ctx context.Context) (UpgradeStatus, error) {
	var resolver = e.newUpgradeResolver()
	var status = UpgradeStatus{Enabled: e.cfg.Schedules.UpgradeInterval.Duration > 0,
		MaxGrabs: e.cfg.Downloader.MaxUpgradeGrabs}

	inflight, err := e.upgradeInflight(ctx)
	if err != nil {
		return status, err
	}
	status.Inflight = inflight
	movies, err := e.store.Movies().ListUpgradeable(ctx, upgradeListLimit)
	if err != nil {
		return status, err
	}
	episodes, err := e.store.Episodes().ListUpgradeable(ctx, upgradeListLimit)
	if err != nil {
		return status, err
	}
	status.Truncated = len(movies) == upgradeListLimit || len(episodes) == upgradeListLimit
	add := func(u upgradeSubject) {
		if !u.belowCutoff() {
			return
		}
		status.Items = append(status.Items, UpgradeItem{SubjectType: u.subject, ID: u.id, Title: u.title,
			Quality: u.quality, Cutoff: u.profile.UpgradeUntil, Profile: u.profile.Name,
			UpgradeChecked: u.checkedAt, State: u.state, Upgrading: u.state != model.StateImported})
	}
	for _, movie := range movies {
		u, err := resolver.movie(ctx, movie)
		if err != nil {
			return status, err
		}
		add(u)
	}
	for _, episode := range episodes {
		u, err := resolver.episode(ctx, episode)
		if err != nil {
			return status, err
		}
		add(u)
	}
	slices.SortStableFunc(status.Items, func(a, b UpgradeItem) int {
		if a.Upgrading != b.Upgrading {
			if a.Upgrading {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Title), strings.ToLower(b.Title))
	})
	return status, nil
}
