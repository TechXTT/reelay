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
4. The winner is added to the downloader with a Reelay-owned category. Only one
   torrent may be active for a series. A season or multi-episode pack creates
   one grab row and atomically reserves every wanted episode it covers; the
   remaining episode searches stop until that grab finishes.
5. The status loop advances progress, detects no-progress stalls, blacklists a
   failed info hash, and returns the item to `wanted` with backoff.
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
cache, and two bounded read connections by default. Indexer JSON is decoded as
a stream, search concurrency is bounded, due queries are limited, and SSE has a
hard client cap. These choices are required for the 256 MB Synology target.

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

The plugin has one shared source tree with exact build targets for Jellyfin
10.11.11 (`net9.0`) and Jellyfin 12 preview (`net10.0`). ABI-specific package
versions are selected at build time; behavioral code is shared.
