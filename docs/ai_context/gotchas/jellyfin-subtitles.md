# Jellyfin subtitle downloads

- **When to read:** Before changing how the Reelay plugin searches or downloads subtitles through Jellyfin's `ISubtitleManager`, or when subtitle jobs stay `waiting` or end as `no_match` unexpectedly.

## Quick Reference

| Symptom or change | Gotcha | Action |
| --- | --- | --- |
| A downloaded track never appears, so jobs end as `track_not_listed` | `ISubtitleManager.DownloadSubtitles` only writes the file. It does not refresh the item, so Jellyfin does not list the new stream. | Call `IProviderManager.QueueRefresh(video.Id, new MetadataRefreshOptions(new DirectoryService(fileSystem)), RefreshPriority.High)` after the download, as Jellyfin's own `SubtitleController` does. |
| A provider outage or limit is recorded as `no_match` | `SearchSubtitles(SubtitleSearchRequest, …)` catches and logs every provider exception and returns an empty array. Only `DownloadSubtitles` rethrows `RateLimitExceededException`. | Treat an empty search as "nothing found", not proof that no subtitle exists. Rate limits are only detectable at download time. |
| A read-only media folder is expected to fail the download | With `SaveSubtitlesWithMedia`, Jellyfin tries the media folder first and falls back to the item's internal metadata folder. It throws (`AggregateException`) only when every path fails. | Expect success with the file stored in metadata. Treat a thrown `IOException` or `UnauthorizedAccessException` as both locations failing. |

## Investigation

<details>
<summary>Evidence and reasoning</summary>

| Order | Observation | What it established |
| --- | --- | --- |
| 1 | `MediaBrowser.Providers/Subtitles/SubtitleManager.cs` at `v10.11.11`: `DownloadSubtitles` calls `TrySaveSubtitle` and nothing else. `Jellyfin.Api/Controllers/SubtitleController.cs` calls `QueueRefresh` right after it. This is the same at `v12.1`. | The caller owns the refresh. |
| 2 | In the same file, `SearchSubtitles` wraps each `provider.Search` in `catch (Exception)` and logs it. `DownloadSubtitles` has `catch (RateLimitExceededException) { throw; }`. | Search errors are invisible to callers. |
| 3 | `TrySaveSubtitle` adds the media-folder path, then the internal metadata path. `TrySaveToFiles` returns on the first successful write and collects exceptions into an `AggregateException`. | A read-only media folder silently falls back to metadata storage. |

</details>
