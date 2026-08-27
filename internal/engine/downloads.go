package engine

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/TechXTT/reelay/internal/downloader"
	"github.com/TechXTT/reelay/internal/store"
)

const downloadsPausedKey = "downloads.paused"

// DownloadsPaused reports the operator-controlled global transfer state.
func (e *Engine) DownloadsPaused(ctx context.Context) (bool, error) {
	return e.downloadsPaused(ctx)
}

func (e *Engine) downloadsPaused(ctx context.Context) (bool, error) {
	value, err := e.store.KV().Get(ctx, downloadsPausedKey)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	paused, err := strconv.ParseBool(value)
	if err != nil {
		return false, errors.New("stored download pause state is invalid")
	}
	return paused, nil
}

// SetDownloadsPaused persists the global state and applies it to all current
// incomplete Reelay grabs. The shared lock prevents a search from adding an
// unpaused torrent between the state write and the bulk client operation.
func (e *Engine) SetDownloadsPaused(ctx context.Context, paused bool) (int, error) {
	e.downloadControlMu.Lock()
	defer e.downloadControlMu.Unlock()

	previous, err := e.downloadsPaused(ctx)
	if err != nil {
		return 0, err
	}
	if err := e.store.KV().Set(ctx, downloadsPausedKey, strconv.FormatBool(paused)); err != nil {
		return 0, err
	}
	grabs, err := e.store.Grabs().Active(ctx)
	if err != nil {
		_ = e.store.KV().Set(context.WithoutCancel(ctx), downloadsPausedKey, strconv.FormatBool(previous))
		return 0, err
	}
	hashes := make([]string, 0, len(grabs))
	seen := make(map[string]bool, len(grabs))
	for _, grab := range grabs {
		hash := strings.ToLower(strings.TrimSpace(grab.TorrentHash))
		if grab.Progress < 1 && hash != "" && !seen[hash] {
			seen[hash] = true
			hashes = append(hashes, hash)
		}
	}
	if err := e.downloader.SetPaused(ctx, hashes, paused); err != nil {
		_ = e.store.KV().Set(context.WithoutCancel(ctx), downloadsPausedKey, strconv.FormatBool(previous))
		return 0, err
	}
	e.events.Publish(Event{Type: "queue_control", At: e.clock.Now().UTC(),
		Data: map[string]any{"paused": paused, "count": len(hashes)}})
	return len(hashes), nil
}

func (e *Engine) addDownload(ctx context.Context, req downloader.AddRequest) (string, error) {
	e.downloadControlMu.Lock()
	defer e.downloadControlMu.Unlock()

	paused, err := e.downloadsPaused(ctx)
	if err != nil {
		return "", err
	}
	req.Paused = req.Paused || paused
	return e.downloader.Add(ctx, req)
}
