// Package downloader defines the contract every download client implements.
//
// The engine depends only on this interface. qBittorrent is the one
// implementation today; Transmission is the planned second one, because
// qBittorrent is not realistically installable on a DSM 6 / Armada 370 NAS and
// topology B needs something that is. Adding it must not require an engine
// change, which is what this interface exists to guarantee.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotFound means the client has no torrent with that hash.
	ErrNotFound = errors.New("torrent not found in the download client")

	// ErrNotOurs means the torrent exists but does not carry one of Reelay's
	// categories.
	//
	// This is the hard safety boundary from the spec, and it is an error rather
	// than a silent skip on purpose. The operator has other torrents in this
	// client. A bug that made Reelay delete one of them would be unrecoverable,
	// so any operation that would touch an unlabelled torrent fails loudly
	// instead of proceeding.
	ErrNotOurs = errors.New("torrent does not carry a Reelay category; refusing to touch it")

	// ErrAuth means the client rejected our credentials.
	ErrAuth = errors.New("download client rejected authentication")
)

// Normalised torrent states. Client-specific strings are mapped onto these so
// the engine's state machine never has to know which client it is talking to.
const (
	StateDownloading = "downloading"
	StateSeeding     = "seeding"
	StateCompleted   = "completed"
	StateStalled     = "stalled"
	StateError       = "error"
	StatePaused      = "paused"
	StateUnknown     = "unknown"
)

// AddRequest is a handoff to the client.
type AddRequest struct {
	Magnet string
	// Category is mandatory. An empty category would make every torrent in the
	// client indistinguishable from ours, so implementations must reject it.
	Category string
	SavePath string
	Paused   bool
}

// TorrentStatus is one torrent as the client currently sees it.
type TorrentStatus struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress float64 `json:"progress"` // 0..1

	// ContentPath is the absolute path to the downloaded file or folder, as the
	// CLIENT sees it. Run it through a PathMapper before opening it.
	ContentPath string `json:"content_path"`

	DownloadedBytes int64         `json:"downloaded_bytes"`
	TotalBytes      int64         `json:"total_bytes"`
	ETA             time.Duration `json:"eta"`

	// Category is carried so callers can re-check ownership rather than trust
	// that a filtered list stayed filtered.
	Category string `json:"category"`

	Seeders  int `json:"seeders"`
	Leechers int `json:"leechers"`

	AddedAt     time.Time `json:"added_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`

	// ErrorMessage is set when State is StateError.
	ErrorMessage string `json:"error_message,omitempty"`
}

// Complete reports whether the payload is fully downloaded and ready to import.
//
// Deliberately requires both a finished-looking state and full progress: a
// torrent can sit in a seeding state while still verifying, and importing a
// half-checked file produces a corrupt library entry.
func (s TorrentStatus) Complete() bool {
	switch s.State {
	case StateSeeding, StateCompleted:
		return s.Progress >= 1
	default:
		return false
	}
}

// Failed reports a state the engine should give up on rather than wait out.
func (s TorrentStatus) Failed() bool { return s.State == StateError }

func (s TorrentStatus) String() string {
	return fmt.Sprintf("%s [%s %.1f%%]", s.Name, s.State, s.Progress*100)
}

// Downloader is a torrent client.
type Downloader interface {
	// Add hands a magnet to the client and returns the info hash it will be
	// tracked by.
	Add(ctx context.Context, req AddRequest) (hash string, err error)

	// Status reports on the given hashes. Torrents that do not carry one of
	// our categories are omitted, never returned.
	Status(ctx context.Context, hashes []string) ([]TorrentStatus, error)

	// Remove deletes a torrent. It must verify our category first and return
	// ErrNotOurs otherwise.
	Remove(ctx context.Context, hash string, deleteData bool) error

	// SetPaused changes transfer state for the requested torrents. Concrete
	// clients must filter the hashes through Reelay's owned categories before
	// issuing the mutation.
	SetPaused(ctx context.Context, hashes []string, paused bool) error

	Healthy(ctx context.Context) error
}
