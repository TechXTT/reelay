# Reelay

A single-binary release watcher and download automation service. It keeps a
queue of wanted movies and a watchlist of series, searches indexers for
matching releases, scores them against a quality profile, hands the winner to a
torrent client, and files the finished download into a media library layout that
Jellyfin or Plex can read.

Same functional category as Sonarr, Radarr and Prowlarr — one binary, no
runtime, no database server.

**Status: complete initial release.** The parser, TPB indexer, scoring pipeline,
qBittorrent client, metadata refresh, audited engine, importer, REST/SSE API,
and embedded web UI are wired end to end.

## Features

- Movie queue and series monitoring backed by TMDB and TVmaze metadata.
- Explainable scoring with persisted rejection reasons, manual overrides,
  retry backoff, and failed-release blacklisting.
- qBittorrent category isolation, live progress, and stall recovery.
- Hardlink-first imports, verified copy fallback, configurable naming,
  season-pack discovery, subtitle carry-over, and recycle-on-upgrade.
- Versioned REST API, bearer auth, health checks, SSE, and a responsive UI.

## Why Go

- One static binary. `GOOS=linux GOARCH=arm GOARM=7 go build` produces
  something you `scp` onto a NAS with no venv, no `node_modules`, no runtime.
- The SQLite driver is pure Go (`modernc.org/sqlite`), so cross-compiling to
  32-bit ARM stays a one-liner. Do not swap it for `mattn/go-sqlite3`; cgo
  breaks exactly that.
- ~20–40 MB resident for a daemon that has to share 256 MB with DSM.
- A branchy state machine gets compile-time exhaustiveness instead of a runtime
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

## Configuration

`config.example.yaml` is fully commented and is the reference. Two rules worth
knowing before you edit it:

- **Every scalar key is overridable by environment variable**, uppercased with
  dots replaced by underscores: `server.auth_token` →
  `REELAY_SERVER_AUTH_TOKEN`. String lists take a comma-separated value. Lists
  of objects (indexers, profiles, path mappings) are file-only.
- **Validation is exhaustive and fatal.** Unknown keys are rejected, so a typo
  cannot silently do nothing, and every problem in the file is reported at once
  with the offending key named.

Two settings are security-relevant:

- `server.auth_token` is required whenever `server.bind` is not a loopback
  address. Reelay refuses to start otherwise, because this process holds your
  download client's credentials. On loopback an empty token is allowed and
  warns loudly.
- `downloader.category_tv` / `category_movies` are the safety boundary. Reelay
  only ever acts on torrents carrying one of its own categories, so the other
  torrents in your client are invisible to it. Neither may be empty.

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

Jellyfin is a consumer, not an integration. Reelay writes correct filenames into
the library layout and Jellyfin picks them up on its own scan; there is no
Jellyfin API client and there will not be one.

Point one library at each root, with the matching content type:

| Jellyfin library | Content type | Folder          |
| ---------------- | ------------ | --------------- |
| Movies           | Movies       | `//nas/Movies`  |
| Shows            | TV Shows     | `//nas/Series`  |

Two things to check:

- If Jellyfin runs as a Windows service under `LocalSystem`, it cannot reach UNC
  paths at all. Run it as your own user account, or give the share guest read
  access.
- Leave "Real time monitoring" on so new imports appear without waiting for the
  scheduled scan. If you would rather trigger it explicitly, set
  `library.post_import_webhook` to Jellyfin's
  `/Library/Refresh?api_key=...` and Reelay will POST to it after each import.

Existing media with raw release-name folders is left alone — Reelay only writes
files it imports itself, so an untidy library and a Reelay-managed one can
coexist in the same share indefinitely.

## Development

```bash
make build      # or .\make.ps1 build
make test
make lint       # go vet + staticcheck
make cross      # linux/amd64, linux/arm64, linux/armv7, windows/amd64
make bench-mem  # peak RSS over a simulated search lifecycle
```

`make test-race` needs cgo and a **64-bit** host C compiler. A Windows box with
only 32-bit MinGW cannot build it (`cc1.exe: sorry, unimplemented: 64-bit mode
not compiled in`); install MSYS2 UCRT64 or TDM-GCC-64, or let CI cover it — the
GitHub Actions workflow runs `-race` on Linux on every push.

The direct Go dependency set is `modernc.org/sqlite`, `gopkg.in/yaml.v3`, and
`golang.org/x/time`. The standard library supplies routing, gzip, SSE, clients,
filesystem work, and test fakes. The frontend uses TypeScript and Vite only as
development dependencies and has no runtime framework.

See [`docs/architecture.md`](docs/architecture.md) for the state machine and
search-to-import flow.

## Legal

Reelay is a generic automation tool for indexers and download clients. It ships
with no content, no indexer credentials and no preconfigured sources, and it
neither hosts nor distributes anything. It speaks to whatever services you point
it at. Ensuring you have the right to download and store what you queue is the
operator's responsibility, and copyright law varies by jurisdiction.
