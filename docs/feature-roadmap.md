# Reelay feature roadmap

Proposed features to improve how the service works day to day, written on
2026-10-04 after the codebase refactor. Each item is either confirmed missing in
the code at that date or listed as an open gap in
[product-plan.md](product-plan.md). None are implemented yet.

Effort is a rough guess: **S** about a day, **M** a few days, **L** a week or more.

## Recommended order

1. **Batch 1, small reliability fixes:** 1, 2, 4, 6, 20.
2. **Batch 2, measurement:** 19, 16.
3. **Batch 3, larger product steps:** 7, 11, 13.
4. **Everything else** as needed.

## Reliability and downloads

1. **Free-space guard before grabbing (S).** Free space is only reported by the
   setup checks (`internal/api/setup.go`, `internal/fsprobe/space_*.go`). Check
   the download and library paths before handing a torrent to qBittorrent, and
   hold the grab with a clear "low disk" status when space is short. On a small
   NAS, a full disk is the most likely way an import fails.
2. **Download from torrent files, not just magnets (M).** Torznab results that
   only offer a `.torrent` file are skipped today, so many Prowlarr results go
   unused. Fetch the file and pass it to qBittorrent.
3. **Dead-torrent detection by seeders (S).** Mark a grab stalled early when it
   stays at 0 seeds or peers past a short timeout, instead of waiting for the
   full stall timeout.
4. **Automatic retry of the next-best release (S).** When a grab fails or stalls,
   immediately grab the next accepted candidate from the saved results instead of
   waiting for the next search cycle.
5. **Import the existing library (M).** Scan the current movie and TV folders and
   register what is already there, so Reelay doesn't search for media you already
   own and new installs start in the right state.
6. **Duplicate preferred groups in quality profiles (S).** Reject or merge group
   names that differ only by case (e.g. "NTB" and "ntb") when the config is
   validated. Today the score for that group is picked at random (found by
   differential testing during the refactor).

## Release quality

7. **Better-quality upgrades with a cutoff (M).** `upgrade_until` exists. Add a
   "Wanted upgrades" view and a periodic background search that replaces
   already-imported files below the cutoff, with a cap on how many upgrade
   downloads run at once.
8. **Anime episode-number mapping (M).** Metadata comes from TVmaze only. Map
   absolute episode numbers to season/episode numbers (the way XEM or AniDB do
   it) so anime packs match and import correctly.
9. **Score explanation on the dashboard (S).** Show each candidate's score
   breakdown (resolution, source, group, language, HDR) next to the existing
   rejection reasons, so tuning a profile isn't guesswork.
10. **Profile preview (S).** "Test this profile against the last search": re-score
    saved candidates under edited settings before saving them.

## Requests and household use

11. **Separate household logins with roles (L).** Today the operator bearer token
    is the only access boundary. Add per-user logins (or Jellyfin single sign-on)
    with request permissions and quotas; the product plan already lists this as
    future work.
12. **Request approval queue (M).** Optionally require operator approval for
    requests from certain users, or above a size limit.
13. **Per-user notifications (M).** Notify the person who made the request when
    their title is available, through their own ntfy topic or webhook.
    Availability messages already go through a retrying queue (the outbox), but
    each install has one destination.
14. **Calendar of upcoming episodes and releases (M).** Show upcoming air dates
    and when Reelay will start searching (air date plus grace). The metadata is
    already stored.
15. **Trial reminders (S).** Nudge a user who finished a trial but hasn't voted
    Continue or Stop after N days, and optionally stop the trial automatically.

## Recommendations

16. **Measure whether recommendations work (M).** The product plan's success
    measures (request rate, and whether requested titles get watched) aren't
    tracked yet. Record them per user and show them in a small stats panel
    before changing scorer weights.
17. **"Because you watched X" groups (S).** The ranking already stores reasons.
    Group Discover cards by their seed title, so the reason behind each pick is
    visible.
18. **Watchlist import (M).** Pull a TMDB, Trakt or IMDb watchlist in as requests
    or as seeds for recommendations.

## Operations and visibility

19. **Metrics endpoint (S).** Add a `/metrics` endpoint (Prometheus format, or
    JSON) covering queue depth, search and grab success rates, indexer
    circuit-breaker state, import times, memory and SQLite size. Nothing like this
    exists yet.
20. **Scheduled automatic backups with retention (S).** Backup and restore exist
    but only run manually (`--backup`, settings view). Add a nightly snapshot that
    keeps the last N copies.
21. **Activity and history page (S).** A searchable log of grabs, imports,
    failures and state changes across all items. The data is already in
    `state_transitions`.
22. **Bulk actions (S).** Retry, search, delete or change the profile for many
    items at once from the dashboard.
23. **Live end-to-end smoke test (M).** A `--selftest` mode that runs a full
    search → grab → import → Jellyfin sync against a small test release. The
    setup checklist lists complete live journeys as not yet verified.

## Integrations

24. **More download clients (M each).** Transmission or Deluge behind the
    existing `downloader.Downloader` interface. Only qBittorrent is supported now.
25. **Plex support (L).** The README says the library layout works with Plex, but
    recommendations, availability and requests depend on the Jellyfin plugin.
26. **Usenet support (L).** Newznab indexers plus a SABnzbd or NZBGet downloader.
    This is a big scope increase, but it is the most common reason people pick
    the Sonarr/Radarr stack instead.

## Constraints for every item

1. Keep the single static binary, pure-Go SQLite, CGO off, and bounded memory
   for the 256 MB NAS target (see [architecture.md](architecture.md)).
2. Never edit applied migrations; add new numbered migrations instead.
3. Validate every new configuration key exhaustively, like the existing config.
4. Keep the downloader ownership boundary: Reelay only touches torrents in its
   own category.
