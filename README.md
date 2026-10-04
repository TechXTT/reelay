# Reelay

A single-binary release watcher and download automation service. It keeps a
queue of wanted movies and a watchlist of series, searches indexers for
matching releases, scores them against a quality profile, hands the winner to a
torrent client, and files the finished download into a media library layout that
Jellyfin or Plex can read.

Same functional category as Sonarr, Radarr and Prowlarr — one binary, no
runtime, no database server.

**Status: implemented and activated locally; complete live journeys remain to verify.**
Discover previews, request tracking and recovery, Torznab support, recommendation
controls, and backup/restore are implemented. Follow the
[activation checklist](docs/setup-checklist.md) to configure and run this build.

## Features

1. Movie queue and series monitoring backed by TMDB and TVmaze metadata.
2. Explainable scoring with persisted rejection reasons, manual overrides,
  retry backoff, and failed-release blacklisting.
3. qBittorrent category isolation, live progress, and stall recovery.
4. Hardlink-first imports, verified copy fallback, configurable naming,
  season-pack discovery, subtitle carry-over, and recycle-on-upgrade.
5. Versioned REST API, bearer auth, health checks, SSE, and a responsive UI.
6. Explainable per-user recommendations and an optional Jellyfin plugin that
  exposes missing titles as Discover libraries and routes requests to Reelay.
7. Discover previews with descriptions, TMDB audience ratings, cast, runtime,
   and click-to-play YouTube trailers or teasers.
8. Specific-season requests, withdrawal and retry, paginated Requests,
   persisted diagnostics, and accepted-release selection.
9. Per-user language and genre preferences, familiarity/diversity controls,
   dismissal undo, editable personal ratings, and sync freshness.
10. Pirate Bay and magnet-capable Torznab sources, including Prowlarr.
11. Availability webhooks, playable Jellyfin links, setup/path checks, free-space
    visibility, and consistent SQLite backups with validated restore.
12. Series trials: one episode, three episodes, or the first season, followed by
    a required Continue/Stop vote and a personal rating from 1 to 5.

In Discover, select a Jellyfin user, switch to Series, and choose **Try 1 episode**,
**Try 3 episodes**, or **Try first season** before requesting a title. Trials start
from the beginning and skip specials. Short series use up to three known episodes;
a season trial includes all known episodes of the first season. Future episodes
wait for their air date and grace period.

The updated Jellyfin plugin checks each user's watched episodes every minute.
Discover keeps a trial panel until that user watches the selected episodes and
submits both a Continue/Stop decision and a rating. Continue automatically monitors
the full series, including future episodes. Stop ends that user's request and
leaves other users' monitoring and shared downloads intact. The vote is required
to expand that trial; it does not block browsing or playback of other titles.
Trial tracking requires Jellyfin plugin v0.1.6 or later.

## Why Go

1. One static binary. `GOOS=linux GOARCH=arm GOARM=7 go build` produces
  something you `scp` onto a NAS with no venv, no `node_modules`, no runtime.
2. The SQLite driver is pure Go (`modernc.org/sqlite`), so cross-compiling to
  32-bit ARM stays a one-liner. Do not swap it for `mattn/go-sqlite3`; cgo
  breaks exactly that.
3. ~20–40 MB resident for a daemon that has to share 256 MB with DSM.
4. A branchy state machine gets compile-time exhaustiveness instead of a runtime
  surprise where an item silently stalls in a state nobody handles.

## Quickstart

```bash
cp config.example.yaml config.yaml
# edit library.tv_root, library.movie_root, downloader.* and the indexer base_url
./reelay --config config.yaml --check   # validate config + schema, then exit
./reelay --config config.yaml           # serve
```

On Windows use `make.ps1` instead of `make` (GNU make on Windows resolves
recipes through the WSL `sh` on PATH, which does not share the Windows
filesystem view):

```powershell
.\make.ps1 web
.\make.ps1 build
.\make.ps1 check
.\make.ps1 run
```

Verify it is alive:

```bash
curl -s localhost:7878/api/v1/ping
curl -s -H "Authorization: Bearer $REELAY_SERVER_AUTH_TOKEN" localhost:7878/api/v1/health
```

Open `http://127.0.0.1:7878/` for the embedded UI.

For an existing installation, preserve the database before running this build:
`--check` applies migrations, and `--backup` also migrates before taking its
snapshot. Follow the [upgrade and setup checklist](docs/setup-checklist.md)
for the shutdown backup, Prowlarr credentials, and Jellyfin setup.

### Docker quickstart

The Compose file mounts qBittorrent and Reelay into the same `media` volume so
hardlinks work. Configure paths under `/media`, set the downloader URL to
`http://qbittorrent:8080`, then run:

```bash
docker compose up -d --build
docker compose logs -f reelay
```

The DS214se has no Docker package. Use the ARMv7 binary there; Compose is for
the desktop topology and newer NAS hosts.

### Flags

| Flag        | Meaning                                            |
| ----------- | -------------------------------------------------- |
| `--config`  | Path to the config file (default `config.yaml`)    |
| `--dev`     | Text logs at debug level instead of JSON at `info` |
| `--check`   | Validate config and apply migrations, then exit    |
| `--version` | Print version and exit                             |
| `--search`  | Run a one-shot parsed and scored indexer search    |
| `--grab`    | Hand one magnet to qBittorrent and follow status   |
| `--list-items` | List persisted movies, series, and episodes  |
| `--backup` | Write a consistent database snapshot to a new file, then exit |
| `--restore` | Validate a backup and restore to an absent configured database path |

## Configuration

`config.example.yaml` is fully commented and is the reference. Two rules worth
knowing before you edit it:

1. **Scalar keys outside object lists are overridable by environment variable**,
   uppercased with
  dots replaced by underscores: `server.auth_token` →
  `REELAY_SERVER_AUTH_TOKEN`. String lists take a comma-separated value. Lists
  of objects (indexers, profiles, path mappings) are file-only.
2. **Validation is exhaustive and fatal.** Unknown keys are rejected, so a typo
  cannot silently do nothing, and every problem in the file is reported at once
  with the offending key named.

Two settings are security-relevant:

1. `server.auth_token` is required whenever `server.bind` is not a loopback
  address. Reelay refuses to start otherwise, because this process holds your
  download client's credentials. On loopback an empty token is allowed and
  warns loudly.
2. `downloader.category_tv` / `category_movies` are the safety boundary. Reelay
  only ever acts on torrents carrying one of its own categories, so the other
  torrents in your client are invisible to it. Neither may be empty.

Series downloads are serialized per series. Reelay starts at most one torrent
for a series at a time. A selected season or multi-episode pack appears once in
the queue, reserves all wanted episodes it contains, and imports only those
reserved episodes. Multi-season packs receive an over-fetch penalty so a
bounded season pack wins when both can satisfy the current season.

For Prowlarr, set the indexer's `type: torznab`, use its endpoint as `base_url`
(for example `http://127.0.0.1:9696/1/api`), and set
`api_key_env: REELAY_PROWLARR_API_KEY`. Supply that variable to the Reelay
process. The Prowlarr key and TMDB key are separate credentials. Torznab accepts
video results with a usable magnet or infohash; torrent-file-only results are
skipped. See the [configuration checklist](docs/setup-checklist.md#configure-prowlarr)
for the full entry and remote-host addressing.

## Storage

SQLite, one file, WAL mode. Timestamps are stored as fixed-width RFC3339 UTC so
lexicographic comparison equals chronological comparison and the values are
readable in the `sqlite3` CLI. Enumerations are `TEXT` with `CHECK` constraints,
so a typo'd state is a write error rather than a row no `switch` handles.

Migrations are numbered `.sql` files in `migrations/`, embedded in the binary and
applied on startup, each in its own transaction. Applied migrations are
checksummed: editing one after it has run is a fatal startup error, and a
database containing a migration the binary does not know about (a downgrade) is
refused rather than operated on.

**Keep `database.path` on local disk.** SQLite WAL relies on shared-memory
locking that SMB and NFS do not provide. Reelay warns if the path looks
networked.

Settings provides **Download database backup**. The CLI also supports
`reelay --config config.yaml --backup backups/reelay.db`; the destination must
not exist. Restore requires stopping Reelay and using a new database destination.
It validates SQLite integrity and migration checksums and refuses to overwrite
an existing file. See [backup and restore](docs/setup-checklist.md#backup-and-restore).
Keep configuration and media backups separately; the snapshot contains SQLite state.

## Hardlinking, and why it is probed at startup

Imports hardlink by default so the torrent client keeps seeding the same bytes
that are now in your library — no second copy, no re-upload. That only works
when the download folder and the library are on the same filesystem.

The catch on a NAS: **hardlinks cannot cross SMB shares**, even when both shares
live on one physical volume, because SMB expresses a link relative to the share
root. So each `downloader.save_path_*` has to live *inside* the library share it
feeds.

Reelay probes this on startup — one temp file, one `link()`, one `os.SameFile`
inode comparison, then cleanup — and reports the verdict in the log and on
`/api/v1/health`. Some SMB and FUSE layers satisfy `link()` by copying, which
looks like success and quietly doubles your disk use; the inode check catches
that. Finding out at startup rather than three hours into a download is the whole
point.

### NAS layout that works

```
//nas/Series/                      <- library.tv_root, and the Jellyfin "Shows" library
    .reelay-downloads/             <- downloader.save_path_tv  (+ an empty .ignore file)
    Some Show (2019)/Season 01/...

//nas/Movies/                      <- library.movie_root, and the Jellyfin "Movies" library
    .reelay-downloads/             <- downloader.save_path_movies  (+ .ignore)
    Some Film (2024)/...
```

The dot prefix plus an empty `.ignore` file keeps Jellyfin and Plex from
scanning in-progress downloads while still letting them scan the library around
it.

Write UNC paths in YAML with **forward slashes** (`//nas/Series`) or in single
quotes (`'\\nas\Series'`). In double-quoted YAML a backslash is an escape
character, so `"\\nas\Series"` is a bug waiting to happen.

`library.recycle_dir` is a bare folder name, not a path: it is created inside
whichever library root holds the file being replaced, so an upgrade is a rename
instead of a copy. With two roots on two shares, one absolute recycle path could
only ever be same-volume for one of them.

For the Synology, mount one NFS export containing both the library and its
download folder. NFS supports `link()` in this layout and is the recommended
setup. SMB/CIFS hardlinks are unreliable; Reelay will report the startup probe
as degraded and use copy+verify, which doubles space and network traffic.

### Remote path mapping

If Reelay and the download client run on different machines, or the client runs
in a container, the path the client reports is not the path Reelay can open.
`downloader.path_mappings` translates prefixes, longest match first. Prefix
matching is case-insensitive and slash-agnostic on Windows, because a
containerised qBittorrent reports POSIX paths that a Windows Reelay has to
recognise.

## Jellyfin

Jellyfin remains the consumer of imported media, and the optional plugin adds a
discovery and request surface without changing the import pipeline. Reelay owns
recommendations and requests; the plugin synchronizes library activity, creates
per-user virtual libraries, and treats Favorite as Request and Dislike as
Dismiss inside those virtual libraries. Playback never requests a title.
Currently owned and previously watched titles are excluded before ranking.

Point one library at each root, with the matching content type:

| Jellyfin library | Content type | Folder          |
| ---------------- | ------------ | --------------- |
| Movies           | Movies       | `//nas/Movies`  |
| Shows            | TV Shows     | `//nas/Series`  |

Two things to check:

1. If Jellyfin runs as a Windows service under `LocalSystem`, it cannot reach UNC
  paths at all. Run it as your own user account, or give the share guest read
  access.
2. Leave "Real time monitoring" on so new imports appear without waiting for the
  scheduled scan. If you would rather trigger it explicitly, set
  `library.post_import_webhook` to Jellyfin's
  `/Library/Refresh?api_key=...` and Reelay will POST to it after each import.

Existing media with raw release-name folders is left alone — Reelay only writes
files it imports itself, so an untidy library and a Reelay-managed one can
coexist in the same share indefinitely.

### Recommendation plugin

Recommendations require `metadata.tmdb_api_key` and
`recommendations.enabled: true`. Install the plugin from the catalog matching
your server, configure its Reelay URL and bearer token, test the connection,
then enable recommendation sync on the plugin page:

1. Jellyfin 10.11 catalog:
  `https://github.bozhilov.me/reelay/manifest.json`
2. Jellyfin 12 catalog:
  `https://github.bozhilov.me/reelay/manifest-preview.json`

The plugin configuration page shows two paths for every enabled Jellyfin user.
Add each path manually under Dashboard → Libraries, once as Movies and once as
Shows, and grant access only to that user. Jellyfin library permissions operate
at library level, which is why each user needs separate Discover libraries.

After enabling the plugin, run **Sync Reelay Recommendations** once from
Dashboard -> Scheduled Tasks so the per-user folders exist before you add them
as libraries. The plugin then performs a full sync at startup and every six hours. Favorites and
dislikes are checked every minute and written to a durable retry outbox before
being sent. Movie requests enter the wanted queue immediately. Series requests
from the plugin use `future_only` monitoring by default; change the series to
`all` in Reelay if you want its aired back catalogue. In Reelay's Discover view,
choose Latest season, All episodes, Future episodes, or Specific seasons before
requesting a series. Season 0 selects specials. Episodes wait for their air date
plus the configured grace period. Existing broader monitoring is preserved when
a narrower scope is requested for the same shared series.

The Requests view pages through requests in batches of 100 for the selected
synchronized Jellyfin user, even after the recommendation leaves Discover. It
shows download and import status, errors, retry timing, and series episode counts. Imported
media and availability confirmed by Jellyfin library sync are separate signals;
a series present in Jellyfin does not mean all its episodes are available.
The user selector is an operator filter under Reelay's shared bearer token,
not a separate login or permission boundary.

Use **Attention needed** to filter persisted failures and **Diagnostics** to
inspect recent search history, retry timing, import errors, and candidate rejection
reasons. **Select release** is available for accepted candidates from the last
search. **Withdraw request** ends that user's subscription and pending notifications;
shared downloads continue. **Retry** schedules eligible failed or waiting subjects.

Set `availability.jellyfin_servers` to map the synchronized plugin server ID to
your Jellyfin base URL for **Open in Jellyfin** links. An optional
`availability.webhook_url` receives availability events after a completed library
sync. Delivery retries with a stable `X-Reelay-Event-ID`; receivers must deduplicate
that ID. See [availability setup](docs/setup-checklist.md#configure-availability).
For phone/browser notifications through ntfy, set `availability.webhook_format:
ntfy` and use your topic URL as `webhook_url`. Reelay sends a readable title
availability message directly, retaining the outbox retries. The default format
is JSON for other receivers.

Use the 1-5 selector on Reelay's Discover view to rate a suggestion without
requesting it. The suggestion is removed, high ratings become recommendation
seeds, and low ratings subtract from matching genres, keywords, and people.
The plugin also synchronizes Jellyfin's native per-user rating field when the
active Jellyfin client exposes a personal-rating control.

**Preferences** controls original languages, excluded genres, familiarity, and
the diversity bonus. Save, then **Refresh** Discover to apply them. Empty filters,
Balanced familiarity, and a 100% diversity bonus preserve the scorer defaults.
**Rating & dismissal history** lets you change personal ratings or undo an unrated
dismissal. Rated titles remain excluded from suggestions.

**Preview** shows the full description and available trailer choices. The TMDB
audience rating is out of 10; your personal rating is out of 5; the match score
measures recommendation fit. YouTube loads only when you choose playback.

The Jellyfin targets are pinned in `plugin/Directory.Build.props`. The Jellyfin
12 artifact is built against Jellyfin 12.1.0; use it with a 12.1 server.

## Development

```bash
make build      # or .\make.ps1 build
make test
make lint       # go vet + staticcheck
make cross      # linux/amd64, linux/arm64, linux/armv7, windows/amd64
make plugin     # Jellyfin 10.11 and 12 plugin ZIPs
make test-all   # Go plus both plugin test targets
make bench-mem  # peak RSS over a simulated search lifecycle
```

`make test-race` needs cgo and a **64-bit** host C compiler. A Windows box with
only 32-bit MinGW cannot build it (`cc1.exe: sorry, unimplemented: 64-bit mode
not compiled in`); install MSYS2 UCRT64 or TDM-GCC-64, or let CI cover it — the
GitHub Actions workflow runs `-race` on Linux on every push.

The direct Go dependency set is `modernc.org/sqlite`, `gopkg.in/yaml.v3`, and
`golang.org/x/time`. The standard library supplies routing, gzip, SSE, clients,
filesystem work, and test fakes. The frontend uses TypeScript and Vite only as
development dependencies and has no runtime framework. Plugin production
dependencies are the exact `Jellyfin.Controller` and `Jellyfin.Model` packages;
xUnit and the .NET test SDK are test-only. Building the plugin requires the
.NET 10 SDK; the 10.11 artifact targets `net9.0` and the 12 artifact targets
`net10.0`.

This monorepo is licensed under GPL-3.0. The Go service and plugin share one
version and GitHub release, but remain separate runtime artifacts because
Jellyfin must load the plugin assembly inside its own process.

See [`docs/architecture.md`](docs/architecture.md) for the state machine and
search-to-import flow, and [`docs/product-plan.md`](docs/product-plan.md) for the
product improvement plan and delivery status. The
[setup checklist](docs/setup-checklist.md) covers activation, live verification,
and backup/restore.

## Legal

Reelay is a generic automation tool for indexers and download clients. It ships
with no content or indexer credentials. The example configuration includes a
public indexer endpoint; you choose and configure the sources. Reelay
neither hosts nor distributes anything. It speaks to whatever services you point
it at. Ensuring you have the right to download and store what you queue is the
operator's responsibility, and copyright law varies by jurisdiction.
