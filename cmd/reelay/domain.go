package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/parser"
	"github.com/TechXTT/reelay/internal/store"
)

type domainCLIOptions struct {
	AddMovie     string
	MovieYear    int
	AddSeries    string
	MonitorMode  string
	AddEpisode   string
	EpisodeTitle string
	AirDate      string
	ListItems    bool
	Transition   string
	Reason       string
	History      string
}

func (o domainCLIOptions) Active() bool {
	return o.AddMovie != "" || o.AddSeries != "" || o.AddEpisode != "" ||
		o.ListItems || o.Transition != "" || o.History != ""
}

func (o domainCLIOptions) actionCount() int {
	n := 0
	for _, active := range []bool{o.AddMovie != "", o.AddSeries != "", o.AddEpisode != "",
		o.ListItems, o.Transition != "", o.History != ""} {
		if active {
			n++
		}
	}
	return n
}

func runDomainCLI(ctx context.Context, st *store.Store, cfg *config.Config, o domainCLIOptions) error {
	if o.actionCount() != 1 {
		return errors.New("choose exactly one of --add-movie, --add-series, --add-episode, --list-items, --transition, or --history")
	}
	profile, err := st.Profiles().Default(ctx)
	if err != nil {
		return err
	}

	switch {
	case o.AddMovie != "":
		movie, err := st.Movies().Create(ctx, model.Movie{
			Title: o.AddMovie, SortTitle: parser.SortTitle(o.AddMovie), Year: o.MovieYear,
			ProfileID: profile.ID, RootFolder: cfg.Library.MovieRoot, State: model.StateWanted,
		}, "movie added from CLI")
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "added movie:%d %q (%d) state=%s profile=%q\n",
			movie.ID, movie.Title, movie.Year, movie.State, profile.Name)
		return nil

	case o.AddSeries != "":
		mode := model.MonitorMode(o.MonitorMode)
		if !mode.Valid() {
			return fmt.Errorf("invalid --monitor-mode %q", o.MonitorMode)
		}
		series, err := st.Series().Create(ctx, model.Series{
			Title: o.AddSeries, SortTitle: parser.SortTitle(o.AddSeries),
			MonitorMode: mode, Status: model.SeriesFollowing,
			ProfileID: profile.ID, RootFolder: cfg.Library.TVRoot,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "added series:%d %q monitor=%s profile=%q\n",
			series.ID, series.Title, series.MonitorMode, profile.Name)
		return nil

	case o.AddEpisode != "":
		seriesID, season, number, err := parseEpisodeSpec(o.AddEpisode)
		if err != nil {
			return err
		}
		var air *time.Time
		if o.AirDate != "" {
			v, err := time.Parse("2006-01-02", o.AirDate)
			if err != nil {
				return fmt.Errorf("parse --air-date: %w", err)
			}
			air = &v
		}
		ep, err := st.Episodes().Create(ctx, model.Episode{
			SeriesID: seriesID, Season: season, Number: number,
			Title: o.EpisodeTitle, AirDate: air, State: model.StateWanted,
		}, "episode added from CLI")
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "added episode:%d series:%d S%02dE%02d state=%s\n",
			ep.ID, ep.SeriesID, ep.Season, ep.Number, ep.State)
		return nil

	case o.ListItems:
		return printItems(ctx, st)
	case o.Transition != "":
		subject, id, state, err := parseTransitionSpec(o.Transition)
		if err != nil {
			return err
		}
		v, err := st.Transitions().Transition(ctx, subject, id, state, o.Reason, "manual CLI transition")
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "%s:%d %s -> %s (%s)\n", subject, id, v.From, v.To, v.Reason)
		return nil
	case o.History != "":
		subject, id, err := parseSubjectSpec(o.History)
		if err != nil {
			return err
		}
		return printHistory(ctx, st, subject, id)
	}
	return nil
}

var episodeSpecRE = regexp.MustCompile(`^(\d+):[sS](\d{1,3})[eE](\d{1,4})$`)

func parseEpisodeSpec(raw string) (int64, int, int, error) {
	m := episodeSpecRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, 0, 0, errors.New(`--add-episode must be formatted as <series-id>:SxxEyy`)
	}
	seriesID, _ := strconv.ParseInt(m[1], 10, 64)
	season, _ := strconv.Atoi(m[2])
	number, _ := strconv.Atoi(m[3])
	return seriesID, season, number, nil
}

func parseTransitionSpec(raw string) (model.SubjectType, int64, model.ItemState, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 3 {
		return "", 0, "", errors.New(`--transition must be formatted as <episode|movie>:<id>:<state>`)
	}
	subject, id, err := parseSubjectSpec(parts[0] + ":" + parts[1])
	if err != nil {
		return "", 0, "", err
	}
	state := model.ItemState(parts[2])
	if !state.Valid() {
		return "", 0, "", fmt.Errorf("invalid item state %q", state)
	}
	return subject, id, state, nil
}

func parseSubjectSpec(raw string) (model.SubjectType, int64, error) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return "", 0, errors.New(`item must be formatted as <episode|movie>:<id>`)
	}
	subject := model.SubjectType(parts[0])
	if !subject.ValidItem() {
		return "", 0, fmt.Errorf("invalid item type %q", parts[0])
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, fmt.Errorf("invalid item id %q", parts[1])
	}
	return subject, id, nil
}

func printItems(ctx context.Context, st *store.Store) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tID\tSTATE/MONITOR\tTITLE")
	movies, err := st.Movies().List(ctx)
	if err != nil {
		return err
	}
	for _, m := range movies {
		fmt.Fprintf(w, "movie\t%d\t%s\t%s (%d)\n", m.ID, m.State, m.Title, m.Year)
	}
	series, err := st.Series().List(ctx)
	if err != nil {
		return err
	}
	for _, s := range series {
		fmt.Fprintf(w, "series\t%d\t%s\t%s\n", s.ID, s.MonitorMode, s.Title)
		episodes, err := st.Episodes().ListBySeries(ctx, s.ID)
		if err != nil {
			return err
		}
		for _, e := range episodes {
			fmt.Fprintf(w, "episode\t%d\t%s\t%s S%02dE%02d %s\n",
				e.ID, e.State, s.Title, e.Season, e.Number, e.Title)
		}
	}
	return w.Flush()
}

func printHistory(ctx context.Context, st *store.Store, subject model.SubjectType, id int64) error {
	values, err := st.Transitions().History(ctx, subject, id, 100)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AT\tFROM\tTO\tREASON")
	for _, v := range values {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.At.Format(time.RFC3339), v.From, v.To, v.Reason)
	}
	return w.Flush()
}
