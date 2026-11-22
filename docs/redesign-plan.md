# Reelay redesign plan

## First delivery

These changes are committed (`27a2062`..`83c7537`). Verification uses isolated service fixtures and the production web build. Confirmation against a deployed Jellyfin library is a separate step.

1. **Let the viewer change direction.** Give Discover a one-click way to look beyond the genre dominating the first ten picks. Query the wider catalog, respect language choices, genre exclusions, library ownership and previous feedback, and keep the regular list available. A viewer can preview, rate, dismiss or request an alternative and return to their original picks.
2. **Keep recoverable downloads.** If qBittorrent reports an ambiguous error, preserve the transfer and its files, surface the reason, and resume tracking after recovery. Distinguish a failed indexer query from a completed search with no suitable release. An outage must not consume the title's search timeout.
3. **Recover at the import step.** Let a requester retry an import from its recorded completed download. Record the episodes that successfully landed even when a pack is incomplete. Keep a new search available when the original files are gone.
4. **Make the Jellyfin connection observable.** Record successful catalog and per-user activity sync independently. Isolate queued action failures so one stale request cannot stop another household member's actions. Use series-level wording in Requests and ntfy notifications when only the parent has been seen.

## Second delivery: episode readiness

Committed (`038f8e7`, `0476364`, `d6cfe02`, `ca87151`); see request recovery items 7 and 8 in [architecture](architecture.md).

1. **Confirm the requested episode for the requester.** The plugin announces `episode-confirmation` in a sync header, leaving the strictly decoded sync body unchanged for older Reelay servers. Reelay lists the requested-scope episodes it imported that Jellyfin has not confirmed for that user, honoring selected seasons and trials. The plugin resolves each one under the series with the user's access, excluding virtual missing-episode placeholders, during each scheduled sync and every minute in its action monitor. Reelay accepts only confirmations that match a current candidate (migration 0014).
2. **Use one piece of evidence everywhere.** While the plugin confirms episodes, a series parent no longer queues a series request's event. The first confirmation queues one global and one per-user event carrying the season, episode and episode item. The ntfy message names the episode (`Show S02E01 is ready to watch in Jellyfin.`) and its Click link opens that episode, on the global ntfy path too. Requests shows `S02E01 ready in Jellyfin` with a **Play S02E01** link. Older plugins keep the series-level "Series listed in Jellyfin" behavior.
3. **Keep confirmations current.** A confirmation older than six hours returns to the plugin as a recheck; episodes the plugin no longer finds for the viewer are withdrawn, and a later confirmation never notifies again.
4. **Confirm movies per requester.** With per-user library data (third delivery), a movie request is available, notified and linked only once the movie is in the requester's own library.
5. **Robust plugin sync.** Trial-progress and confirmation failures are logged per user instead of stopping the recommendation refresh, and both share one lazy series scan per pass.

## Third delivery

Committed through merges `efaf85a`, `63196de` and `3a0dd06`, implemented in parallel worktrees and verified together after merging.

1. **Choose something for tonight.** The plugin reports which catalog items each user can access, replaced atomically per pass (migration 0015). A Tonight view lists only those titles with Play in Jellyfin, session-only filters for media type, time available and unfinished titles, and Save for later (up to 200 titles). Older plugins show an update prompt instead of the server catalog. Nothing on the page requests or downloads; acquiring stays in Discover.
2. **Broaden everyday recommendations.** Each pick keeps its seed or catalog source and names it in the first reason. A diversity re-rank (blocks of 10, at most 3 per seed and 5 per genre by default, configurable) runs before storage so Discover and the plugin agree. Top-ten concentration is recorded per generation (migration 0016) and shown next to requests, watching and completion in Recommendation outcomes. Discover offers session-only steering (more or less like a title, genre or seed) that re-ranks immediately without writing anything; saved preferences remain the only lasting channel. In the animation-heavy fixture the top-ten Animation share fell from 100% to 50% and distinct interests rose from 5 to 7.
3. **Make service-wide problems visible on individual requests.** Failures with one cause become one operator incident (migration 0017) for indexers, the download client, storage paths, library roots, TVmaze and shared notification receivers, resolved automatically on recovery and kept across restarts. The dashboard and `GET /api/v1/incidents` show them, and metrics export open counts. Each request tells the viewer what Reelay waits for, when it checks again and whether they need to act, without hosts, paths, trackers or raw errors; members never receive raw error text.

**Verified** (on the merged tree):

1. go build, vet, `go test ./...` (store schema test at 17, changed under the approval given for 0014), gofmt, TypeScript, `npm run build`, and `.\make.ps1 plugin-test` on Jellyfin 10.11.11 and 12.1.0.
2. Temporary fixtures in scratch copies outside the repository:
   1. Episode acceptance case, recheck withdrawal without renotifying, and per-user movie readiness, with a second user on the server-level path.
   2. Tonight visibility, atomic replacement, filters, scoping, Save for later, cascade and the old-plugin fallback.
   3. Diversity, provenance, concentration trends, steering without persistence, and household isolation.
   4. Incidents for indexer, storage, library and download client outages, across two members and a restart, with viewer-safe waits.

**Not verified:** a live Jellyfin server and plugin install (user-access filtering, placeholder exclusion, poster loading), a live qBittorrent or indexer outage, and the new views in a browser.

**Remaining:**

1. When two real series share a TMDB id, only the first is checked for trials and confirmations. This can miss a confirmation but never invents one.
2. Notifications stay once per request, for the first confirmed episode; later episodes of a followed series do not notify.
3. Tonight's "unfinished" filter uses append-only activity, so a title marked unplayed later still counts as finished. Posters load directly from the Jellyfin base URL.
4. Steering re-ranks at most the top 100 stored rows, and `boost_seed` has no control in the UI yet. Diversity reorders the existing candidates; it does not fetch new ones.
5. Indexer and storage incidents resolve only on a search pass with due items; an incident for an indexer removed from config stays open until resolved another way. A completed download whose source path is unavailable shows a viewer wait but no incident, and TMDB failures are not tracked.
6. The concentration snapshot is written after the recommendations, not in the same transaction.

## Acceptance journeys

1. Start with ten animation-heavy picks. Change direction, request a non-animation title, and return to the original list. Verify the user's saved preferences, Jellyfin Discover library, and unrelated household profiles retain their regular picks.
2. Interrupt the indexer and the download client separately while a request is active. Restore each connection and restart Reelay during a hold. Verify retained progress and search budget, one active transfer, and a visible reason for the wait.
3. Request a pilot, finish downloading it, and temporarily make the library unavailable. Restore access and retry import. Verify the same completed files land successfully. With the episode-confirmation plugin, verify exactly one ready notification opens the pilot for the requester; fixture-verified, live check pending.
4. Import a pack missing one requested episode. Verify the successful episodes remain imported, the missing episode has a recovery action, and a different household member's request remains intact.
