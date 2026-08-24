package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/TechXTT/reelay/internal/clock"
	"github.com/TechXTT/reelay/internal/config"
	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/indexer"
	"github.com/TechXTT/reelay/internal/metadata"
	"github.com/TechXTT/reelay/internal/recommendation"
	"github.com/TechXTT/reelay/internal/store"
)

type Importer interface {
	ImportCompleted(context.Context, int64) error
}

type Options struct {
	Store           *store.Store
	Config          *config.Config
	Indexers        []indexer.Indexer
	Downloader      downloader.Downloader
	TVmaze          metadata.SeriesProvider
	Importer        Importer
	PathMapper      *downloader.PathMapper
	Clock           clock.Clock
	Logger          *slog.Logger
	Events          *EventBus
	Recommendations *recommendation.Service
}

type Engine struct {
	store      *store.Store
	cfg        *config.Config
	indexers   []indexer.Indexer
	downloader downloader.Downloader
	tvmaze     metadata.SeriesProvider
	importer   Importer
	pathMapper *downloader.PathMapper
	clock      clock.Clock
	log        *slog.Logger
	events     *EventBus
	searchSem  chan struct{}

	searchTrigger         chan struct{}
	statusTrigger         chan struct{}
	metadataTrigger       chan struct{}
	recentTrigger         chan struct{}
	recommendationTrigger chan struct{}
	recommendations       *recommendation.Service
}

func New(opt Options) (*Engine, error) {
	if opt.Store == nil || opt.Config == nil || opt.Downloader == nil {
		return nil, errors.New("engine requires store, config, and downloader")
	}
	if opt.Clock == nil {
		opt.Clock = clock.Real{}
	}
	if opt.Logger == nil {
		opt.Logger = slog.Default()
	}
	if opt.Events == nil {
		opt.Events = NewEventBus(opt.Config.Runtime.MaxSSEClients)
	}
	concurrency := opt.Config.Runtime.SearchConcurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	return &Engine{
		store: opt.Store, cfg: opt.Config, indexers: opt.Indexers,
		downloader: opt.Downloader, tvmaze: opt.TVmaze, importer: opt.Importer,
		pathMapper: opt.PathMapper,
		clock:      opt.Clock, log: opt.Logger, events: opt.Events,
		searchSem:     make(chan struct{}, concurrency),
		searchTrigger: make(chan struct{}, 1), statusTrigger: make(chan struct{}, 1),
		metadataTrigger: make(chan struct{}, 1), recentTrigger: make(chan struct{}, 1),
		recommendationTrigger: make(chan struct{}, 1), recommendations: opt.Recommendations,
	}, nil
}

func (e *Engine) Events() *EventBus { return e.events }

func (e *Engine) Trigger(loop string) error {
	var ch chan struct{}
	switch loop {
	case "search":
		ch = e.searchTrigger
	case "status":
		ch = e.statusTrigger
	case "metadata":
		ch = e.metadataTrigger
	case "recent":
		ch = e.recentTrigger
	case "recommendations":
		ch = e.recommendationTrigger
	default:
		return fmt.Errorf("unknown engine loop %q", loop)
	}
	select {
	case ch <- struct{}{}:
	default:
	}
	return nil
}

func (e *Engine) Run(ctx context.Context) error {
	type loopSpec struct {
		name     string
		interval time.Duration
		trigger  <-chan struct{}
		run      func(context.Context) error
	}
	loops := []loopSpec{
		{"search", e.cfg.Schedules.SearchInterval.Duration, e.searchTrigger, e.SearchOnce},
		{"status", e.cfg.Schedules.StatusInterval.Duration, e.statusTrigger, e.StatusOnce},
		{"metadata", e.cfg.Schedules.MetadataInterval.Duration, e.metadataTrigger, e.MetadataOnce},
		{"recent", e.cfg.Schedules.RecentInterval.Duration, e.recentTrigger, e.RecentOnce},
	}
	if e.cfg.Recommendations.Enabled && e.recommendations != nil {
		loops = append(loops, loopSpec{"recommendations", e.cfg.Recommendations.RefreshInterval.Duration, e.recommendationTrigger, e.recommendations.GenerateAll})
	}
	var wg sync.WaitGroup
	for _, spec := range loops {
		spec := spec
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticks, stop := e.clock.NewTicker(spec.interval)
			defer stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticks:
				case <-spec.trigger:
				}
				if err := spec.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					e.log.Error("engine loop failed", "loop", spec.name, "error", err)
				}
			}
		}()
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (e *Engine) publish(kind string, target searchTarget, data map[string]any) {
	e.events.Publish(Event{Type: kind, At: e.clock.Now().UTC(),
		SubjectType: string(target.subject), SubjectID: target.id, Data: data})
}
