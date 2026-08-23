package qbittorrent

import (
	"strings"
	"time"

	"github.com/TechXTT/reelay/internal/downloader"
)

// infoRow is one element of a torrents/info response.
//
// The field set was taken from a live qBittorrent 4.4.1 response rather than
// from the documentation. Only the fields Reelay uses are declared; the API
// sends about fifty, and decoding the rest would be churn every time upstream
// adds one.
type infoRow struct {
	Hash         string  `json:"hash"`
	InfohashV1   string  `json:"infohash_v1"`
	Name         string  `json:"name"`
	Category     string  `json:"category"`
	State        string  `json:"state"`
	Progress     float64 `json:"progress"`
	ContentPath  string  `json:"content_path"`
	SavePath     string  `json:"save_path"`
	Size         int64   `json:"size"`
	TotalSize    int64   `json:"total_size"`
	Downloaded   int64   `json:"downloaded"`
	Completed    int64   `json:"completed"`
	AmountLeft   int64   `json:"amount_left"`
	ETA          int64   `json:"eta"`
	NumSeeds     int     `json:"num_seeds"`
	NumLeechs    int     `json:"num_leechs"`
	AddedOn      int64   `json:"added_on"`
	CompletionOn int64   `json:"completion_on"`
}

// etaInfinite is the sentinel qBittorrent uses for "no meaningful estimate"
// (100 days in seconds). Reported verbatim it would render as an ETA of 2400
// hours, which is worse than reporting none.
const etaInfinite = 8640000

func (r infoRow) toStatus() downloader.TorrentStatus {
	// Prefer infohash_v1: for a hybrid v1+v2 torrent, `hash` can be the v2
	// hash, and the v1 form is what our magnets and our database key on.
	hash := strings.ToLower(strings.TrimSpace(r.InfohashV1))
	if hash == "" {
		hash = strings.ToLower(strings.TrimSpace(r.Hash))
	}

	// qBittorrent reports total_size as -1 for a magnet whose metadata has not
	// been fetched yet, which rendered as "-1 B" in the progress line. A
	// negative size means unknown, not a size.
	total := r.TotalSize
	if total <= 0 {
		total = r.Size
	}
	if total < 0 {
		total = 0
	}

	st := downloader.TorrentStatus{
		Hash:            hash,
		Name:            r.Name,
		State:           normaliseState(r.State),
		Progress:        clampProgress(r.Progress),
		ContentPath:     contentPath(r),
		DownloadedBytes: max64(r.Downloaded, 0),
		TotalBytes:      total,
		Category:        r.Category,
		Seeders:         r.NumSeeds,
		Leechers:        r.NumLeechs,
	}

	if r.ETA > 0 && r.ETA < etaInfinite {
		st.ETA = time.Duration(r.ETA) * time.Second
	}
	if r.AddedOn > 0 {
		st.AddedAt = time.Unix(r.AddedOn, 0).UTC()
	}
	if r.CompletionOn > 0 {
		st.CompletedAt = time.Unix(r.CompletionOn, 0).UTC()
	}
	if st.State == downloader.StateError {
		st.ErrorMessage = errorDetail(r.State)
	}
	return st
}

// contentPath falls back to save_path + name when content_path is absent.
// content_path arrived in 4.x; an older client or an odd mirror may omit it,
// and an empty path would make the importer look in the working directory.
func contentPath(r infoRow) string {
	if p := strings.TrimSpace(r.ContentPath); p != "" {
		return p
	}
	if r.SavePath == "" || r.Name == "" {
		return ""
	}
	sep := "/"
	if strings.Contains(r.SavePath, `\`) {
		sep = `\`
	}
	return strings.TrimRight(r.SavePath, `/\`) + sep + r.Name
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func clampProgress(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 1:
		return 1
	default:
		return p
	}
}

// normaliseState maps qBittorrent's state vocabulary onto ours.
//
// Both the 4.x and 5.x spellings are handled: 5.0 renamed pausedDL/pausedUP to
// stoppedDL/stoppedUP. Supporting both means one binary works across the
// upgrade rather than silently reporting every paused torrent as unknown.
//
// The DL/UP suffix is the important part of each name — it says which side of
// completion the torrent is on, which is precisely what the engine needs.
func normaliseState(s string) string {
	switch s {
	// Failed outright.
	case "error", "missingFiles", "unknown":
		return downloader.StateError

	// Finished, and actively (or nominally) seeding.
	case "uploading", "forcedUP", "queuedUP", "checkingUP":
		return downloader.StateSeeding
	// Finished, seeding, but with nobody to seed to. Still complete, and the
	// importer only cares about completion.
	case "stalledUP":
		return downloader.StateSeeding
	// Finished and deliberately not seeding.
	case "pausedUP", "stoppedUP":
		return downloader.StateCompleted

	// Still fetching.
	case "downloading", "forcedDL", "queuedDL", "checkingDL", "metaDL",
		"forcedMetaDL", "allocating", "checkingResumeData", "moving":
		return downloader.StateDownloading
	// Fetching, but with no peers. Distinct from downloading because this is
	// the state a stall timeout is watching for.
	case "stalledDL":
		return downloader.StateStalled
	// Deliberately not fetching.
	case "pausedDL", "stoppedDL":
		return downloader.StatePaused

	default:
		// An unrecognised state is reported as unknown rather than guessed at.
		// Guessing "downloading" would make the engine wait forever on
		// something that will never finish.
		return downloader.StateUnknown
	}
}

func errorDetail(rawState string) string {
	switch rawState {
	case "missingFiles":
		return "the download client reports missing files; the data was moved or deleted outside the client"
	case "error":
		return "the download client reports an error on this torrent"
	case "unknown":
		return "the download client reports an unknown state"
	default:
		return "the download client reports state " + rawState
	}
}
