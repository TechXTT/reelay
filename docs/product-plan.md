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
4. Following deliveries: planned; implementation has not started. Notifications,
   arbitrary season selection, request cancellation/retry, diagnostic candidate
   selection, and older-request pagination remain outside this first delivery.
