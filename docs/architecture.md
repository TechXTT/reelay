# Architecture

Reelay is one Go process with an embedded Vite application and a local SQLite
database. Concrete clients are constructed in `cmd/reelay`; the engine only
depends on the `Indexer`, `Downloader`, metadata, and importer interfaces.
The same repository also contains a separately built C# Jellyfin plugin. It is
a filesystem/API bridge and does not contain recommendation or download logic.

## Search to import

1. The metadata loop refreshes followed TVmaze series and creates episode rows.
   Monitoring rules move an episode to `wanted` only after air time plus grace.
2. The search loop selects due rows, groups them by title, and queries healthy
   indexers under a bounded semaphore. Every item is protected by a leased
   SQLite advisory lock.
3. Parser output passes through hard filters before accepted releases receive a
   weighted score. All accepted and rejected decisions are persisted.
4. The winner is checked against the free space of the library and download
   volumes (`downloader.min_free_space_mb` reserve); if it does not fit, the item
   is held for the next search interval without counting a failed attempt. The
   winner is then added to the downloader with a Reelay-owned category. Only one
   torrent may be active for a series. A season or multi-episode pack creates
   one grab row and atomically reserves every wanted episode it covers; the
   remaining episode searches stop until that grab finishes.
5. The status loop advances progress, detects no-progress stalls, blacklists a
   failed info hash, and returns the item to `wanted` with backoff. A failed or
   stalled grab immediately tries the next accepted candidate from the last
   search (within 24h, at most 3 times) before waiting for the next search.
6. At completion the importer maps the client path, discovers media files,
   hardlinks or checksum-copies only the reserved episodes into the library,
   carries subtitles, and commits every covered `importing -> imported`
   transition with its own final path.
7. Each transition and progress update is published to the bounded SSE bus. The
   database remains authoritative if a browser misses an event.

## State machine

```text
unmonitored -> wanted -> searching -> grabbed -> downloading -> importing -> imported
                  ^          |             |                         |
                  |          +-- no match -+                         +-> import_failed
                  +-- retry/backoff/stall -------------------------------+

wanted/searching -> failed after the configured give-up period
failed/import_failed/imported -> wanted only by retry or upgrade
```

Every edge is validated in `model.ItemState.CanTransitionTo` and recorded in
`state_transitions` with a reason. Loops use compare-and-update writes plus item
leases, so duplicate manual and ticker invocations remain idempotent.

## Storage and memory

SQLite runs in WAL mode with one writer connection, a small configurable page
cache, and two bounded read connections by default. Indexer JSON and Torznab XML
are decoded as streams, search concurrency is bounded, due queries are limited,
and SSE has a hard client cap. These choices are required for the 256 MB Synology target.

## Filesystem boundary

Configuration validation rejects unsafe roots. The importer resolves every
destination relative to its configured root before creating, replacing, or
deleting anything. Startup probes hardlinks between each mapped download path
and library root. NFS with both paths in one export is recommended; SMB commonly
falls back to a verified copy.

## Recommendation flow

1. The Jellyfin plugin pages through real Movie and Series items, excludes its
   own virtual paths, and sends external IDs plus per-user completion/favorite/
   like signals in bounded, idempotent batches. A sync token marks items absent
   only after the final batch succeeds, so an interrupted sync cannot erase the
   current inventory.
2. Reelay selects at most 12 recent positive seeds and asks TMDB for recommended
   and similar candidates. Cold-start profiles use TMDB Discover.
3. Owned, watched, requested, dismissed, rated, and duplicate titles are removed before a
   bounded set of at most 300 candidates is ranked.
4. The deterministic scorer combines TMDB confidence, content and people
   affinity, multi-seed evidence, rating confidence, user preferences, and a
   small diversity bonus. Components and plain-language reasons are persisted.
5. The plugin materializes the best 40 movies and series beneath isolated
   per-user paths. Jellyfin supplies normal artwork and metadata from the TMDB
   provider IDs embedded in their filenames.
6. Favoriting a virtual item creates an idempotent Reelay request; disliking it
   dismisses it. A durable plugin outbox retries failures. The item disappears
   from Discover after success or once the real media enters the library.
7. A 1-5 rating is durable per user and title. Ratings above neutral add
   recommendation seeds; ratings below neutral reduce matching taste signals.
8. A separate media request records each requester and links to the shared
   movie or series. Recommendation refresh does not remove this attribution.
   The operator's Requests view combines persisted subject state and episode
   counts with availability confirmed by that Jellyfin server's library sync.
   Imported media and confirmed Jellyfin presence remain separate signals.
9. Dashboard requests can choose latest-season, all, future-only monitoring, or
   explicit seasons including specials. Selected seasons are stored separately
   and unioned into shared series monitoring after air date plus grace.
   Latest-season monitoring selects the most recently aired numbered season;
   older plugin clients keep their future-only default. Shared monitoring may
   widen when a new requester asks for more episodes, and does not narrow.
10. Preview details and videos are fetched from TMDB on demand and cached
    separately from recommendation-generation metadata. Stored audience ratings,
    personal ratings, and match scores remain distinct. Only validated YouTube
    video IDs are exposed, and the browser loads playback after user interaction.
11. Per-user language and genre filters apply before and after enrichment.
    Familiarity and diversity choices adjust that user's ranking weights;
    unconfigured preferences retain the defaults. Dismissal undo removes the
    dismissal exclusion; rated titles remain excluded and their ratings are editable.
12. Series trials store per-request scopes and selected episode IDs separately
    from the shared monitor. Metadata selects the first one or three numbered
    episodes, or all known episodes of the first numbered season, excluding
    specials. Short series cap the three-episode target at their known count.
    The plugin reports each requester's played episodes every minute. Only a
    completed trial accepts a Continue/Stop vote with a 1-5 rating. Continue
    widens monitoring to all episodes; Stop withdraws only that request. Request
    summaries and recovery controls stay limited to the trial's selected episodes
    until continuation. The operator bearer token remains the authorization boundary.

## Request recovery and availability

1. Request attribution remains independent of the shared media subject and
   recommendation card. Requests uses bounded pagination, and selected-season
   summaries avoid counting another user's unrelated episodes or failures.
2. Withdrawal cancels the requester's subscription and removes undelivered
   availability events. It does not stop shared downloads. Replaying an old
   request action preserves withdrawal; a new request action can reactivate it.
3. Diagnostics reads persisted transitions and candidate evaluations. It exposes
   recent search timing, retry timing, attempts, and import errors. Release
   selection validates subject identity and permits accepted candidates from
   the most recent search; active-transfer safeguards remain in the engine.
4. A completed Jellyfin sync inserts an availability event once per active
   request into the SQLite outbox. The notification loop leases due events,
   POSTs JSON to the configured receiver, and retries failures with backoff.
   A stable event ID supports receiver deduplication under at-least-once delivery.
5. Playable links use the synchronized item ID and a configured Jellyfin base
   URL keyed by the plugin's server ID. Series title presence and episode import
   counts are shown separately. Availability is not inferred from import success.

## Source and maintenance boundaries

1. Torznab supports generic search and recent listings for magnet-capable movie
   and TV results. XML size and result count are bounded, requests are rate-limited
   and timed out, and failures update the existing circuit breaker. Results flow
   through the existing parser and scorer; no download client was added.
2. Setup checks examine the database, downloader connectivity, library/path
   presence, and free space. Indexer health remains a local circuit-breaker check,
   so dashboard polling cannot create indexer traffic.
3. Backups use SQLite `VACUUM INTO` for a consistent snapshot. Restore opens the
   source read-only, validates integrity and known migration checksums, and
   creates a new destination without overwriting existing data. Scheduled backups
   (`database.backup_interval`) write timestamped snapshots to `backup_dir` and
   keep the newest `backup_keep`. Configuration and media files are outside the
   database snapshot.
4. The dashboard retains operator access through the existing bearer token.
   Selecting a Jellyfin user filters data; it does not create separate household
   authentication or quota enforcement.

The plugin has one shared source tree with exact build targets for Jellyfin
10.11.11 (`net9.0`) and Jellyfin 12 preview (`net10.0`). ABI-specific package
versions are selected at build time; behavioral code is shared.

Availability delivery supports the default JSON event contract and direct ntfy
text messages through `availability.webhook_format`. Both use the same outbox,
stable event ID, bounded HTTP call, and retry schedule.

Use the [activation checklist](setup-checklist.md) for configuration, shutdown
backups before migration, plugin setup, and live verification.
