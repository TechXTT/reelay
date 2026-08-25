package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/model"
	"github.com/TechXTT/reelay/internal/scoring"
)

type searchTarget struct {
	subject     model.SubjectType
	id          int64
	seriesID    int64
	want        model.Wanted
	episodes    []model.Episode
	profile     model.QualityProfile
	runtime     int
	attempts    int
	firstWanted *time.Time
	category    string
	savePath    string
	imported    *scoring.Imported
}

func (e *Engine) dueTargets(ctx context.Context) ([]searchTarget, error) {
	now := e.clock.Now().UTC()
	movies, err := e.store.Movies().WantedDue(ctx, now, 500)
	if err != nil {
		return nil, err
	}
	episodes, err := e.store.Episodes().WantedDue(ctx, now, 1000)
	if err != nil {
		return nil, err
	}
	out := make([]searchTarget, 0, len(movies)+len(episodes))
	profiles := map[int64]model.QualityProfile{}
	profile := func(id int64) (model.QualityProfile, error) {
		if p, ok := profiles[id]; ok {
			return p, nil
		}
		p, err := e.store.Profiles().Get(ctx, id)
		if err == nil {
			profiles[id] = p
		}
		return p, err
	}
	for _, movie := range movies {
		p, err := profile(movie.ProfileID)
		if err != nil {
			return nil, err
		}
		out = append(out, searchTarget{subject: model.SubjectMovie, id: movie.ID,
			want:    model.Wanted{Kind: model.SubjectMovie, Title: movie.Title, Year: movie.Year},
			profile: p, runtime: movie.RuntimeMinutes, attempts: movie.SearchAttempts,
			firstWanted: movie.FirstWantedAt, category: e.cfg.Downloader.CategoryMovies,
			savePath: e.cfg.Downloader.SavePathMovies, imported: importedQuality(movie.ImportedQuality)})
	}
	seriesCache := map[int64]model.Series{}
	episodeCache := map[int64][]model.Episode{}
	for _, episode := range episodes {
		series, ok := seriesCache[episode.SeriesID]
		if !ok {
			series, err = e.store.Series().Get(ctx, episode.SeriesID)
			if err != nil {
				return nil, err
			}
			seriesCache[episode.SeriesID] = series
		}
		p, err := profile(series.ProfileID)
		if err != nil {
			return nil, err
		}
		seriesEpisodes, ok := episodeCache[series.ID]
		if !ok {
			seriesEpisodes, err = e.store.Episodes().ListBySeries(ctx, series.ID)
			if err != nil {
				return nil, err
			}
			episodeCache[series.ID] = seriesEpisodes
		}
		wantedNumbers := make([]int, 0, len(seriesEpisodes))
		wantedEpisodes := make([]model.Episode, 0, len(seriesEpisodes))
		for _, candidate := range seriesEpisodes {
			if candidate.State != model.StateWanted {
				continue
			}
			wantedEpisodes = append(wantedEpisodes, candidate)
			if candidate.Season == episode.Season {
				wantedNumbers = append(wantedNumbers, candidate.Number)
			}
		}
		out = append(out, searchTarget{subject: model.SubjectEpisode, id: episode.ID, seriesID: series.ID,
			want: model.Wanted{Kind: model.SubjectEpisode, Title: series.Title,
				Aliases: series.Aliases, Season: episode.Season, Episode: episode.Number,
				AbsoluteEp: episode.AbsoluteNumber, IsAnime: series.IsAnime,
				WantedEpisodes: wantedNumbers},
			episodes: wantedEpisodes,
			profile:  p, runtime: series.RuntimeMinutes, attempts: episode.SearchAttempts,
			firstWanted: episode.FirstWantedAt, category: e.cfg.Downloader.CategoryTV,
			savePath: e.cfg.Downloader.SavePathTV, imported: importedQuality(episode.ImportedQuality)})
	}
	return out, nil
}

func importedQuality(raw string) *scoring.Imported {
	fields := strings.Fields(strings.ToLower(raw))
	if len(fields) < 2 {
		return nil
	}
	quality := &scoring.Imported{Resolution: fields[0], Source: fields[1]}
	for _, field := range fields[2:] {
		quality.Proper = quality.Proper || field == "proper"
		quality.Repack = quality.Repack || field == "repack"
	}
	return quality
}

func targetKey(t searchTarget) string {
	return fmt.Sprintf("%s:%t", strings.ToLower(strings.TrimSpace(t.want.Title)), t.want.IsAnime)
}

func targetQuery(t searchTarget) string {
	if t.subject == model.SubjectMovie && t.want.Year > 0 {
		return fmt.Sprintf("%s %d", t.want.Title, t.want.Year)
	}
	return t.want.Title
}

func targetCategories(t searchTarget) []int {
	if t.subject == model.SubjectMovie {
		return []int{indexer.CatMovies, indexer.CatMoviesDVDR, indexer.CatMoviesHD}
	}
	cats := []int{indexer.CatTVShows, indexer.CatTVShowsHD}
	if t.want.IsAnime {
		cats = append(cats, indexer.CatVideoOther)
	}
	return cats
}
