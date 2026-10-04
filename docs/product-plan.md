# Reelay product improvement plan

Reelay should make the journey from personalized Jellyfin discovery to playable
media understandable and dependable, while retaining its single-binary service
and bounded resource use on a small NAS.

## First delivery: request scope, tracking, and recovery

1. Let dashboard users choose latest season, all episodes, or future episodes
   when requesting a recommended series. Explain the scope before submission.
   Preserve the existing future-only default for older Jellyfin plugin clients.
   Shared series downloads must not lose broader monitoring when another user
   requests a narrower scope.
2. Persist requester attribution separately from recommendation cards. Add a
   user-filtered Requests view that survives recommendation refresh and shows
   backend state, download progress, errors, retry timing, and series import
   counts. Several users may follow one shared movie or series download.
3. Distinguish imported media from availability confirmed by Jellyfin library
   sync. A series present in Jellyfin does not prove every episode was imported;
   keep the episode counts visible.
4. Recover downloads that stop progressing after the beginning of a transfer.
   Reconcile torrents that disappear from the download client after a grace
   period. Preserve intentional pauses and the downloader ownership boundary.
5. Refresh authoritative dashboard state after SSE reconnect. Give Add, Search,
   recommendation refresh, profile changes, and manual triggers visible pending
   and error feedback, preventing duplicate submissions while requests run.
6. Run Go tests and vet, frontend typechecking and production build,
   and targeted smoke checks with isolated fixtures. Update migration assertions
   and add focused regression tests with the user's explicit approval. Do not
   modify dependencies, system configuration, or existing applied migrations. Preserve
   pre-existing working-tree edits; leave staging and commits to the user.

Acceptance: a dashboard user can choose an existing monitoring scope, request a
title, find it again under Requests, and understand whether it is waiting,
downloading, imported, available in Jellyfin, or needs attention. Download stalls
at intermediate progress and missing torrents no longer remain active forever.

This first delivery does not include arbitrary individual-season selection,
notifications, separate end-user authentication, or a full diagnostic workspace.
The Requests user selector is an operator filter under the existing bearer token,
not an authorization boundary between household members.

## Following deliveries

1. **Complete the request journey.** Add specific-season selection, explicit
   request cancellation and retry, durable availability notifications through a
   generic webhook, and a link to the actual playable Jellyfin title. Record
   availability transitions once and deliver notifications through a retryable
   outbox. Add request permissions and quotas if household self-service requires
   them.
2. **Make failures actionable.** Add an Attention needed view with last search,
   next retry, candidate rejection reasons, import errors, and release selection.
   Reuse persisted evaluations and transitions instead of inferring explanations
   in the browser.
3. **Improve source coverage.** Add a Torznab adapter for compatible indexer
   services, retaining bounded concurrency, rate limits, circuit breakers, and
   explainable scoring. Validate live protocol behavior before selecting any new
   dependency or broadening supported download clients.
4. **Improve recommendation control.** Add explicit language preferences, genre
   exclusions, familiarity/diversity controls, dismissal undo, editable ratings,
   and recommendation freshness and sync status. Measure results before changing
   scorer weights or adding a different recommendation system.
5. **Reduce setup and maintenance work.** Provide guided connection and path
   checks with corrective actions, simpler per-user Discover library setup,
   free-space visibility, and a supported SQLite backup and restore workflow.

## Discover delivery: choosing what to watch

1. Show TMDB audience ratings out of 10 and their vote counts, with the
   recommendation match score and the user's personal rating labeled separately.
   Store audience ratings in the existing recommendation features without a new
   database migration. Older cards gain stored ratings on recommendation refresh;
   opening a preview shows the current cached TMDB rating immediately.
2. Add an on-demand preview for movies and series with the full description,
   genres, runtime, cast and filmmakers, and available trailers or teasers.
   Use the existing metadata cache and request timeout. Keep video lookups out of
   the recommendation-generation loop and prefer official trailers.
3. Embed YouTube previews only after the user chooses playback, provide a direct
   YouTube link, and show explicit missing-video and retry states. Closing the
   dialog cancels the pending lookup and stops video playback.

## Measures of success

1. Request-to-availability success rate and elapsed time, with waiting for an
   unaired episode distinguished from a stalled download.
2. Requests requiring manual intervention and their recorded failure reasons.
3. Recommendation requests and subsequent watch activity per user, using the
   already synchronized Jellyfin signals.
4. Time to the first successful request after setup and peak memory on the NAS
   target during search, recommendation generation, and import.

## Delivery status

1. First delivery: implemented and verified on 2026-10-03. Migration 0006 stores
   requester attribution and backfills legacy recommendation requests whose
   shared media subjects still exist. The Requests feed returns the latest 100
   records for the selected user; older records remain stored. Explicit series
   scope, monitoring widening, replay validation, and latest-aired-season
   selection are implemented. Download recovery covers intermediate stalls,
   disappearance grace, pause/resume grace, and completed import recovery;
   queued and maintenance states are exempt from download stall timeouts.
2. Verification: `go test ./...`, `CGO_ENABLED=1 go test -race ./...`,
   `go vet ./...`, `npm run build`, formatting checks on changed Go files,
   `git diff --check`, and `tool-use docs-list --check` pass. Focused tests cover
   request attribution, replay safety, shared scope, deleted subject identity,
   Jellyfin availability, migration backfill, latest season, and download
   recovery. Browser smoke checks used an isolated database and local provider
   fixtures: series requests persist, user filters separate requests, sync
   availability updates over SSE, failed searches re-enable controls with
   visible feedback, health 503 still renders diagnostics, and narrow/wide
   layouts have no horizontal overflow.
3. Verification limits: no live Jellyfin or qBittorrent instance was changed or
   exercised. LSP navigation was unavailable because `gopls` is missing; the
   LSP tool's empty diagnostics result is not proof of successful analysis.
   Staticcheck is unavailable on this machine. Compiler, vet, and race checks
   supplied the local code verification; no tools were installed.
4. Discover delivery: implemented and verified on 2026-10-04. Existing Go tests,
   vet, and frontend typechecking and production build pass. Browser smoke checks
   with intercepted API fixtures verify movie and series previews, ratings,
   complete descriptions, trailer selection, click-to-load playback, missing
   trailers, lookup retry, keyboard close and focus return, cancelled lookups,
   invalid preview responses, and a mobile layout without horizontal overflow.
   Live TMDB responses and live YouTube playback were not exercised. The custom
   browser CLI could not locate Chrome on Windows; Playwright supplied the browser
   checks. No tests, dependencies, or machine configuration were changed.
5. Request journey and diagnostics: implemented on 2026-10-04. Migration 0007
   adds selected seasons and a durable availability outbox. Selected seasons
   include specials and wait for air date plus grace; shared monitoring only
   widens. Request summaries and diagnostics respect the selected seasons.
   Requests supports pagination, withdrawal, retry, persisted failure history,
   accepted-candidate selection, and a playable Jellyfin link when configured.
   Withdrawal cancels the requester's subscription and pending notifications;
   shared downloads continue. Replaying an old request action preserves a later
   withdrawal. A new request action can reactivate it.
6. Recommendation controls and maintenance: implemented on 2026-10-04. Per-user
   language and genre filters, familiarity and diversity controls, dismissal
   undo, editable personal ratings, and sync/generation freshness are available.
   Unconfigured preferences preserve the scorer defaults. Setup checks show
   connection/path failures with corrective actions and free space; indexer
   health checks describe circuit-breaker status without making search traffic.
   The Jellyfin plugin offers per-user setup steps and copyable library paths.
   SQLite backups use a consistent snapshot; restore validates integrity and
   migration checksums and refuses to overwrite an existing destination.
7. Source coverage: Torznab is implemented and registered after a successful
   live check against the operator's Windows Prowlarr endpoint on 2026-10-04.
   Capabilities, a recent listing with 100 usable magnet video results, and a
   named search passed. The adapter bounds results and XML size, rate-limits
   calls, enforces timeouts, and uses the existing circuit breaker and scoring.
   Torrent-file-only results without a usable infohash remain unsupported.
   Credentials can be read from a named process environment variable; no live
   credentials are stored in tracked files. No downloads were added by the check.
8. Current verification: Go tests and vet, frontend typecheck/production build,
   and Linux ARMv7 cross-compilation pass. Focused tests cover season air/grace
   behavior, scope validation, replay after withdrawal, outbox deduplication and
   webhook retries, preference filtering after metadata enrichment, backup and
   restore, and malformed Torznab responses. Browser fixtures verify preferences,
   rating edits and undo, season input validation, attention filtering,
   pagination, candidate selection, withdrawal, setup free-space display,
   backup downloads, and mobile Requests without horizontal overflow.
   Race verification could not compile: installed GCC reports
   `cc1.exe: sorry, unimplemented: 64-bit mode not compiled in`.
   The initial direct plugin-test command selected a default .NET SDK that did
   not satisfy global.json. A later check found the matching SDK through the
   make.ps1 local fallback; both no-restore target checks failed with
   `NETSDK1005` because the cached assets lacked the required target frameworks.
   Normal restore through the repository launcher subsequently passed both
   plugin test targets (six tests each). No toolchains were installed or changed.
9. Local activation: completed with the user's authorization on 2026-10-04.
   The previous database/configuration/executable were backed up with Reelay
   stopped, then the current UI and executable were built and schema 7 activated.
   Prowlarr uses its existing local credential through a process environment
   variable; the ignored local launcher supplies it on subsequent starts.
   The rebuilt Jellyfin plugin loaded and synchronized 19 real library items for
   two users. qBittorrent health, NAS paths/free space, and a live TMDB preview
   passed. An isolated restore passed integrity/schema checks with matching row
   counts. ntfy support now sends readable messages through the same durable
   outbox, with focused HTTP-format and retry tests. A random ntfy topic is
   configured locally; its cache confirmed a real outbox event and a connection
   confirmation. New media downloads/imports, phone receipt, and actual
   YouTube playback remain to be verified.

## Operating the following deliveries

Follow the [activation checklist](setup-checklist.md). It is the operator guide
for Windows builds, Prowlarr credentials, TMDB and Jellyfin Discover setup,
availability links/webhooks, end-to-end verification, and backup/restore.

The local service and plugin are now running the updated working-tree builds.
For another installation, preserve the old database with Reelay stopped before opening it with the new
binary: startup, `--check`, and `--backup` apply pending migrations. Separate
household authentication, permissions, and quotas remain conditional future work.
