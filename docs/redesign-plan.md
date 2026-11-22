# Reelay redesign plan

## First delivery

These changes are committed (`27a2062`..`83c7537`). Verification uses isolated service fixtures and the production web build. Confirmation against a deployed Jellyfin library is a separate step.

1. **Let the viewer change direction.** Give Discover a one-click way to look beyond the genre dominating the first ten picks. Query the wider catalog, respect language choices, genre exclusions, library ownership and previous feedback, and keep the regular list available. A viewer can preview, rate, dismiss or request an alternative and return to their original picks.
2. **Keep recoverable downloads.** If qBittorrent reports an ambiguous error, preserve the transfer and its files, surface the reason, and resume tracking after recovery. Distinguish a failed indexer query from a completed search with no suitable release. An outage must not consume the title's search timeout.
3. **Recover at the import step.** Let a requester retry an import from its recorded completed download. Record the episodes that successfully landed even when a pack is incomplete. Keep a new search available when the original files are gone.
4. **Make the Jellyfin connection observable.** Record successful catalog and per-user activity sync independently. Isolate queued action failures so one stale request cannot stop another household member's actions. Use series-level wording in Requests and ntfy notifications when only the parent has been seen.

## Second delivery: episode readiness

Implemented in the working tree; see request recovery item 7 in [architecture](architecture.md).

1. **Confirm the requested episode for the requester.** The plugin announces `episode-confirmation` in a sync header, leaving the strictly decoded sync body unchanged for older Reelay servers. Reelay lists the requested-scope episodes it imported that Jellyfin has not confirmed for that user, honoring selected seasons and trials. The plugin resolves each one under the series with the user's access, excluding virtual missing-episode placeholders, during each scheduled sync and every minute in its action monitor. Reelay accepts only confirmations that match a current candidate (migration 0014).
2. **Use one piece of evidence everywhere.** While the plugin confirms episodes, a series parent no longer queues a series request's event. The first confirmation queues one global and one per-user event carrying the season, episode and episode item. The ntfy message names the episode (`Show S02E01 is ready to watch in Jellyfin.`) and its Click link opens that episode, now on the global ntfy path too. Requests shows `S02E01 ready in Jellyfin` with a **Play S02E01** link. Older plugins keep the series-level "Series listed in Jellyfin" behavior.

**Verified:**

1. go build, vet and `go test ./...`. The store schema test now expects version 14, changed with approval.
2. gofmt, `npm run build`, and `.\make.ps1 plugin-test` on Jellyfin 10.11.11 and 12.1.0.
3. Temporary fixtures in a scratch copy, outside the repository, exercised the HTTP API and the notification loop:
   1. Season one is in the library and the viewer requests season two. Repeated syncs of the series parent queue nothing.
   2. The season-two episode becomes a candidate only after import. A season-one confirmation is rejected as out of scope.
   3. The first confirmation queues exactly one global and one per-user event. Global and per-user ntfy each deliver one message that opens the episode.
   4. Repeated confirmations, syncs and delivery passes send nothing more.
   5. A sync without the capability header keeps the series-level event.

**Not verified:** a live Jellyfin server with the plugin installed, including the user-access filter and placeholder exclusion against real libraries.

**Remaining:**

1. Revoke or recheck a confirmation when the episode leaves Jellyfin or the viewer loses access.
2. A confirmation failure during the scheduled plugin sync aborts the rest of that sync, as trial progress already does. Consider making both best-effort.
3. The plugin scans all series once for trials and once for confirmations per user per minute while work is pending. One shared scan would be cheaper.
4. When two real series share a TMDB id, only the first is checked. This can miss a confirmation but never invents one.
5. Movies still rely on parent presence; per-user movie visibility is not confirmed.
6. Notifications stay once per request, for the first confirmed episode. Later episodes of a followed series do not notify.

## Next deliveries

1. **Choose something for tonight.** Add a view for playable items from the viewer's Jellyfin libraries, session-only runtime constraints, and Save for later. Keep acquiring a new title and playing an existing one distinct at the point of action.
2. **Broaden everyday recommendations.** Retain the seed or catalog source behind each candidate. Give distinct interests room in the regular pool and measure top-ten concentration per profile alongside requests and subsequent viewing. Use session steering to guide the next set immediately. Treat an explicit lasting preference separately.
3. **Make service-wide problems visible on individual requests.** Group related storage or provider errors into one operator incident. Each affected viewer should see what the service is waiting for, when it will check again, and whether the viewer needs to act.

## Acceptance journeys

1. Start with ten animation-heavy picks. Change direction, request a non-animation title, and return to the original list. Verify the user's saved preferences, Jellyfin Discover library, and unrelated household profiles retain their regular picks.
2. Interrupt the indexer and the download client separately while a request is active. Restore each connection and restart Reelay during a hold. Verify retained progress and search budget, one active transfer, and a visible reason for the wait.
3. Request a pilot, finish downloading it, and temporarily make the library unavailable. Restore access and retry import. Verify the same completed files land successfully. With the episode-confirmation plugin, verify exactly one ready notification opens the pilot for the requester; fixture-verified, live check pending.
4. Import a pack missing one requested episode. Verify the successful episodes remain imported, the missing episode has a recovery action, and a different household member's request remains intact.
