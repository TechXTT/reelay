# Plan for downloading missing subtitles

## First delivery

Offer an operator a preview of movies and individual episodes that have no selectable subtitle track. Let the operator choose a target language and download a small batch through Jellyfin. Apply the same policy to new imports once Jellyfin has indexed the actual file. Subtitle work should never delay import, playback, or a ready notification.

Use **no selectable subtitles in any language** as the initial rule. An embedded subtitle, an external subtitle, a forced-only track, and a track with an unknown language all count as present. An unscanned file has unknown coverage and waits for inspection. A later **missing my preferred language** mode can define whether forced-only tracks satisfy a request for full dialogue.

## What already exists

1. Reelay's importer copies matching subtitle sidecars from the video's directory, carries their language or other filename suffix into the library, and skips existing destinations. A file with embedded subtitles can therefore be covered even when no sidecar exists. [Importer](../internal/importer/sidecars.go)
2. The Jellyfin plugin already excludes Reelay's virtual Discover entries and identifies real episodes while confirming availability for a viewer. Its sync contract does not report subtitle streams. For an existing-library backfill, enumerate real movies and episodes independently of the request list. Inspect each playable media source so two editions of one title can have different coverage. [Plugin sync](../plugin/Jellyfin.Plugin.Reelay/Services/SyncService.cs) · [Sync contract](../plugin/Jellyfin.Plugin.Reelay/Models/Contracts.cs)
3. Jellyfin already supplies a **Download Missing Subtitles** task and a provider interface. Its downloader checks configured languages and library options. A French-only film can therefore be eligible in a library configured to fetch English, even though the film already has subtitles. The roadmap's release-language selection addresses an earlier decision, when choosing which release to acquire. [Jellyfin task](https://jellyfin.org/docs/general/server/tasks/) · [Jellyfin downloader](https://github.com/jellyfin/jellyfin/blob/master/MediaBrowser.Providers/MediaInfo/SubtitleDownloader.cs) · [Reelay roadmap](feature-roadmap.md)

## Delivery order

1. **Start with a read-only audit.** Pick one movie library and one series library. Read Jellyfin's probed media streams after scanning, group the result by present, missing and unknown, and list the exact movie edition or season and episode for each missing item. Exclude virtual suggestions and inaccessible sources. Check a sample against the subtitle selector in the player. This first step needs no provider account.
2. **Pilot the existing provider.** Check which provider and target languages the operator has configured in Jellyfin. For a handful of audited items, require a confident match to the actual file or episode. Re-read its streams immediately before downloading. Use Jellyfin's configured storage and fetcher, refresh the item afterward, and count success only once the track appears. Surface weak matches for manual selection. If an installed provider cannot distinguish editions, leave the ambiguous item for review. [Jellyfin OpenSubtitles setup](https://jellyfin.org/docs/general/server/plugins/open-subtitles/) · [Subtitle manager interface](https://github.com/jellyfin/jellyfin/blob/master/MediaBrowser.Controller/Subtitles/ISubtitleManager.cs)
3. **Choose how much automation is needed.** If the household actually wants a particular language wherever it is missing, pilot Jellyfin's scheduled task first. For the stricter zero-track rule, implement the eligibility check in the Reelay Jellyfin plugin and invoke the configured provider only for approved items. Coordinate with the library's existing subtitle schedule so two workers do not independently fetch the same tracks. Keep credentials in Jellyfin.
4. **Backfill, then follow new imports.** Start with a bounded operator-triggered batch. Deduplicate work by server, media source and target language across users. Persist enough state to resume a batch after restart. Back off on provider limits or outages, retry no-match results less often, and invalidate coverage after a file replacement. For new imports, enqueue only after Jellyfin has probed the imported media. Show operators counts for confirmed, waiting, no match and failed. Expose coverage to a viewer only for items they can access.

## Acceptance checks

1. A movie with an embedded track, an episode with a sidecar, and a file with an unknown-language track produce zero provider calls under the initial rule.
2. An unsubtitled episode receives the selected language for the correct episode. A second run, a restart, or a second household member requesting it produces no duplicate. An alternate edition with different coverage is evaluated separately.
3. A missing provider, exhausted quota, read-only media folder, ambiguous match, or failed library refresh produces a visible outcome. The movie or episode stays playable throughout.
4. An older plugin or pending library scan reports unknown coverage. An upgraded media file is inspected again before a download is proposed.
