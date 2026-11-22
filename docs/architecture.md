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
Transitions older than `runtime.audit_retention` are pruned daily, keeping each
item's latest transition.

Automatic upgrades add no state. When `schedules.upgrade_interval` is above 0, an
hourly loop re-wants imported items whose imported resolution ranks below their
profile's `upgrade_until`, checking each item at most once per interval and
keeping at most `downloader.max_upgrade_grabs` such items in flight. The normal
search loop then looks for a better release; if none is found the item returns
to `imported` with its file untouched, and a successful grab replaces the file
through the recycle directory.

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
    until continuation. A member may vote only on their own trials (see the
    access model in the operations notes below).
13. Each stored refresh records the best 40 titles (what the plugin shows) as
    impressions, keeping the first offer time and score and the best rank.
    `GET /api/v1/recommendations/stats` aggregates, per user and window, how many
    offered titles were requested, dismissed, rated, watched, completed, and
    confirmed available. This measures outcomes before any scorer change.
14. Each candidate keeps up to five sources, strongest first: a seed title and
    the TMDB listing that offered it (`recommendations` or `similar`), or a
    catalog listing (`discover`, `discover_outside_genre`). They are stored in
    `features_json.sources`, returned as `sources`, and name the first reason
    ("Because you liked A and B").
15. Ranking keeps the relevance components and then places titles greedily by
    relevance plus novelty in blocks of `recommendations.diversity_window`. In a
    block, at most `diversity_seed_cap` titles share a strongest seed and at
    most `diversity_genre_cap` share a genre while other candidates remain; a
    demoted title carries a negative `diversity` component so stored scores stay
    ordered and still sum. This runs before storage, so the plugin's top 40 and
    Discover agree. 0 or an absent key uses the defaults 10, 3 and 5; a cap equal
    to the window turns that cap off. When the first block still breaks a cap
    because too few alternatives exist, generation fetches up to
    `recommendations.diversity_extra_pages` TMDB Discover pages outside the
    crowded genre (default 1, -1 off, at most 5; cached 24 hours and shared
    across users), filters them like any candidate, and ranks again.
16. Every generation writes one `recommendation_concentration` row (migration
    0016) for the top ten: dominant genre and its share, dominant source and its
    share, distinct sources and distinct genres. Stats return per-media-type
    averages with the earliest and latest snapshot in the window as
    `concentration`. The snapshot is written in the same transaction as the
    stored list. Rows older than 400 days are pruned.
17. Session steering rides on `GET /api/v1/recommendations` as repeated
    `more_like`, `less_like`, `boost_genre`, `mute_genre`, `boost_seed` and
    `mute_seed` parameters (at most 20 each, `media_type` required). The server
    re-ranks the whole stored active list (up to `result_limit`, at least 100
    rows) with a `steering` component
    scaled by `recommendations.steering_weight`, applies the same diversity
    rules, hides less-like titles, and writes nothing. The browser keeps
    steering in `sessionStorage` per user and media type; saved preferences stay
    the only lasting channel, and a muted genre can be saved as an exclusion
    only through an explicit action. Cards offer "More from" and "Less from"
    their seed title.

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
6. Per-user availability notifications use `notification_destinations` (one per
   Jellyfin user) and a separate `user_availability_outbox`, drained on its own
   even when no global webhook is configured. The same sync that first queues a
   title also queues a user event for requesters with an enabled destination;
   withdrawal removes undelivered rows from both outboxes. Members choose only
   an ntfy topic on the operator-configured `availability.user_ntfy_base_url`;
   arbitrary webhook URLs are operator-only. Disabled or deleted destinations
   close queued events as "destination removed" instead of retrying.
7. Episode readiness. A plugin that sends `X-Reelay-Plugin-Capabilities:
   episode-confirmation` on sync marks its server in `kv`; while it does, a
   present series parent no longer queues a series request's availability
   event. The plugin reads `GET /api/v1/integrations/jellyfin/episode-confirmations`
   (requested-scope episodes Reelay imported and Jellyfin has not confirmed for
   that user), resolves each episode under the series with the user's access
   and without virtual "missing" placeholders, and posts the episode items back.
   Reelay stores only confirmations that match a current candidate in
   `jellyfin_episode_confirmations` (migration 0014). A request's first
   confirmation queues one event with `season`, `episode` and the episode's
   `jellyfin_item_id`, so the notification and the Requests link open that
   episode; both outboxes stay unique per request, so repeated syncs never
   notify again. Requests reports the lowest confirmed episode as
   `ready_season`/`ready_episode`. Older plugins send no header and keep the
   series-level "listed in Jellyfin" signal. A confirmation older than six
   hours returns to the plugin as a `recheck` candidate; the plugin re-reports
   episodes it still finds and lists the rest under `missing`, and Reelay
   withdraws those confirmations. Each batch of new confirmations queues one
   event per request naming the first new episode and `episode_count`;
   `episode_notifications` (migration 0018) records every announced episode, so
   rechecks and re-confirmations never announce one twice. The outboxes are
   unique per (request, item); title-level events keep one per request in code.
   The plugin checks every real series that shares the TMDB id.
8. Per-user movie readiness. When the plugin reports a user's library through
   `POST /api/v1/integrations/jellyfin/user-items` (staged under a sync token and
   made current atomically on `complete`, migration 0015), a movie request is
   available, notified and linked only once the movie is in that user's last
   completed pass; completing a pass queues the event. Users without per-user
   data keep the server-catalog signal.
9. Incidents (migration 0017). Loops record failures and recoveries per cause
   in memory and write once per pass: one open incident per (`kind`,
   `cause_key`) for indexers (by name), the download client, storage (by
   path), library roots, the TVmaze provider, and the shared notification
   receivers. Unchanged incidents are rewritten at most every five minutes; a
   cause seen healthy resolves its incident; open incidents reload after a
   restart; resolved ones are pruned with `runtime.audit_retention` and capped
   at 200. `GET /api/v1/incidents` (operator) and the dashboard show the cause,
   affected count, next step and check time, operator action and raw detail;
   metrics export `reelay_incidents_open{kind}`. Each search pass rechecks free
   space for open storage incidents and, when nothing is due, probes indexers
   with open incidents once (never while the breaker is open). Completed
   downloads whose files are unreachable open `storage` incidents keyed
   `download_files:<mapped path>`, owned by the status loop. TMDB failures in the
   recommendation loop open a `metadata`/`tmdb` incident; interactive calls do
   not. At startup, incidents whose cause left the configuration resolve with a
   `resolved_note`.
10. Each listed request carries viewer-safe `wait_kind`, `waiting_for`,
    `next_check_at`, `needs_action` and `action_hint`, derived from the latest
    transition reason, download waits and open incidents. The copy never names
    hosts, paths, trackers or raw errors, and members never receive an item's
    raw `last_error`; operators still do.

## Source and maintenance boundaries

1. Torznab supports generic search and recent listings for movie and TV results.
   XML size and result count are bounded, requests are rate-limited and timed
   out, and failures update the existing circuit breaker. Results flow through
   the existing parser and scorer; no download client was added. Results with a
   magnet or info hash are grabbed as magnets. Results offering only a `.torrent`
   link are stored with a `download_url` and a stand-in key (SHA-1 of indexer
   name and cleaned URL) as their identity for de-duplication and blacklisting;
   the real info hash is computed from the file at grab time and blacklisted too
   on failure. Such links must share the indexer's scheme, host and port, the
   `apikey` parameter is stripped before storage and re-added only when fetching
   through the indexer's rate limiter and breaker, redirects stay on that host
   (or end in a magnet), and files are limited to 4 MiB.
2. Setup checks examine the database, downloader connectivity, library/path
   presence, and free space. Indexer health remains a local circuit-breaker check,
   so dashboard polling cannot create indexer traffic.
3. Backups use SQLite `VACUUM INTO` for a consistent snapshot. Restore opens the
   source read-only, validates integrity and known migration checksums, and
   creates a new destination without overwriting existing data. Scheduled backups
   (`database.backup_interval`) write timestamped snapshots to `backup_dir` and
   keep the newest `backup_keep`. Configuration and media files are outside the
   database snapshot.
4. Access has two roles. Operators can use every endpoint: the static
   `server.auth_token` (unchanged, for the plugin, Prometheus, scripts and the
   dashboard's token prompt) and, when `auth.logins_enabled` is true, Jellyfin
   administrators who sign in. Members are other Jellyfin users enabled in
   Reelay. Sign-in (`POST /api/v1/auth/login`) forwards the Jellyfin username and
   password to the server named in `availability.jellyfin_servers`, which
   verifies them; Reelay never stores the password and logs the Jellyfin session
   it created out again at once. The session is a random token in the
   `reelay_session` cookie (HttpOnly, SameSite=Strict, Secure behind HTTPS); only
   its SHA-256 is stored in `sessions`, it lasts `auth.session_ttl`, and expired
   rows are pruned daily. Disabling a user in Reelay revokes their session on the
   next request. A cookie-authenticated request that changes state must send
   `X-Reelay-Request: 1`; bearer requests are exempt. Failed sign-ins are limited
   to 5 per 10 minutes per client address (the peer address, never
   `X-Forwarded-For`; behind a reverse proxy all users share the proxy's
   address).
5. Every route declares a level: public (ping, `auth/config`, `auth/login`),
   member, or operator. Members may use only the recommendation, request,
   trial, notification-destination and Tonight endpoints of their own Jellyfin
   user plus `auth/me` and `auth/logout`; everything else, including the event
   stream, metrics, incidents, settings, queue and upgrades, answers 403. A member's scope comes from the session: omitted
   `server_id`/`user_id` are filled in, naming another user is 403, and another
   member's recommendation, request or trial id is 404. Operators still name the
   user and may act for anyone.
6. `auth.member_request_quota` (0 = unlimited) caps how many new requests a member
   creates in `auth.member_quota_window`. The excess answers 429
   `quota_exceeded` with `Retry-After`. Cancelled requests still count, and
   replaying an action or re-requesting a title the member already has is never
   blocked. Operators are not limited.
7. `GET /api/v1/metrics` exposes Prometheus text metrics to operators (the same
   bearer token). Engine counters are in-process and reset on restart; gauges are read
   from SQLite, the indexer breakers and the runtime at scrape time. Label sets
   are fixed, so metric memory does not grow with data.
8. Tonight (`GET /api/v1/tonight`, `GET|POST /api/v1/tonight/saved`,
   `DELETE /api/v1/tonight/saved/{item_id}`) lists only titles in the viewer's
   last completed library pass that are present in the catalog, with
   per-request filters for media type, maximum runtime and unfinished titles
   that the browser keeps in memory only. "Unfinished" uses the played state
   the plugin reports with each library pass (`played_item_ids`, offered only
   when the sync response sets `user_played_supported`), falling back to watch
   activity for passes without it. Without per-user data it returns
   `supported: false` and no items, so the server catalog never leaks. Save for
   later keeps up to 200 titles per user (`saved_for_later`); saved titles the
   viewer can no longer access come back with `playable: false` and no links.
   The page plays existing titles only; acquiring stays in Discover.

The plugin has one shared source tree with exact build targets for Jellyfin
10.11.11 (`net9.0`) and Jellyfin 12.1.0 (`net10.0`). ABI-specific package
versions are selected at build time; behavioral code is shared.

Availability delivery supports the default JSON event contract and direct ntfy
text messages through `availability.webhook_format`. Both use the same outbox,
stable event ID, bounded HTTP call, and retry schedule.

Use the [activation checklist](setup-checklist.md) for configuration, shutdown
backups before migration, plugin setup, and live verification.
