# Reelay redesign plan

## First delivery

These changes are implemented in the working tree. Verification uses isolated service fixtures and the production web build. Confirmation against a deployed Jellyfin library is a separate step.

1. **Let the viewer change direction.** Give Discover a one-click way to look beyond the genre dominating the first ten picks. Query the wider catalog, respect language choices, genre exclusions, library ownership and previous feedback, and keep the regular list available. A viewer can preview, rate, dismiss or request an alternative and return to their original picks.
2. **Keep recoverable downloads.** If qBittorrent reports an ambiguous error, preserve the transfer and its files, surface the reason, and resume tracking after recovery. Distinguish a failed indexer query from a completed search with no suitable release. An outage must not consume the title's search timeout.
3. **Recover at the import step.** Let a requester retry an import from its recorded completed download. Record the episodes that successfully landed even when a pack is incomplete. Keep a new search available when the original files are gone.
4. **Make the Jellyfin connection observable.** Record successful catalog and per-user activity sync independently. Isolate queued action failures so one stale request cannot stop another household member's actions. Use series-level wording in Requests and ntfy notifications when only the parent has been seen.

## Next deliveries

1. **Confirm playback for the requester.** Extend the plugin contract to send accessible movie and episode identities. Match the requested season and episode for trials and selected seasons. When files land, request a targeted confirmation and keep a scheduled reconciliation for missed updates. Notify once when the first requested episode is available to that viewer and link to that episode. Older plugins should report the series as listed until confirmation is available.
2. **Choose something for tonight.** Add a view for playable items from the viewer's Jellyfin libraries, session-only runtime constraints, and Save for later. Keep acquiring a new title and playing an existing one distinct at the point of action.
3. **Broaden everyday recommendations.** Retain the seed or catalog source behind each candidate. Give distinct interests room in the regular pool and measure top-ten concentration per profile alongside requests and subsequent viewing. Use session steering to guide the next set immediately. Treat an explicit lasting preference separately.
4. **Make service-wide problems visible on individual requests.** Group related storage or provider errors into one operator incident. Each affected viewer should see what the service is waiting for, when it will check again, and whether the viewer needs to act.

## Acceptance journeys

1. Start with ten animation-heavy picks. Change direction, request a non-animation title, and return to the original list. Verify the user's saved preferences, Jellyfin Discover library, and unrelated household profiles retain their regular picks.
2. Interrupt the indexer and the download client separately while a request is active. Restore each connection and restart Reelay during a hold. Verify retained progress and search budget, one active transfer, and a visible reason for the wait.
3. Request a pilot, finish downloading it, and temporarily make the library unavailable. Restore access and retry import. Verify the same completed files land successfully. In the episode-confirmation delivery, verify exactly one ready notification opens the pilot for the requester.
4. Import a pack missing one requested episode. Verify the successful episodes remain imported, the missing episode has a recovery action, and a different household member's request remains intact.
