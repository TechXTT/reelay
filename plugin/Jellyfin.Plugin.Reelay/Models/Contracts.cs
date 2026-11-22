using System.Text.Json.Serialization;

namespace Jellyfin.Plugin.Reelay.Models;

public sealed record SyncRequest(
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("sync_token")] string SyncToken,
    [property: JsonPropertyName("complete")] bool Complete,
    [property: JsonPropertyName("users")] IReadOnlyList<SyncUser> Users,
    [property: JsonPropertyName("items")] IReadOnlyList<SyncItem> Items);

public sealed record SyncResponse(
    [property: JsonPropertyName("activity_completion_supported")] bool ActivityCompletionSupported,
    [property: JsonPropertyName("user_library_supported")] bool UserLibrarySupported,
    [property: JsonPropertyName("user_played_supported")] bool UserPlayedSupported);

public sealed record UserItemsRequest(
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("user_id")] string UserId,
    [property: JsonPropertyName("sync_token")] string SyncToken,
    [property: JsonPropertyName("complete")] bool Complete,
    [property: JsonPropertyName("item_ids")] IReadOnlyList<string> ItemIds,
    // Omitted for Reelay versions that do not accept it (their decoding is strict).
    [property: JsonPropertyName("played_item_ids"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] IReadOnlyList<string>? PlayedItemIds);

public sealed record SyncUser(
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("user_id")] string UserId,
    [property: JsonPropertyName("display_name")] string DisplayName,
    [property: JsonPropertyName("enabled")] bool Enabled,
    [property: JsonPropertyName("last_synced_at")] DateTime LastSyncedAt);

public sealed record SyncItem(
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("item_id")] string ItemId,
    [property: JsonPropertyName("media_type")] string MediaType,
    [property: JsonPropertyName("tmdb_id")] int TmdbId,
    [property: JsonPropertyName("tvdb_id")] int TvdbId,
    [property: JsonPropertyName("imdb_id")] string ImdbId,
    [property: JsonPropertyName("title")] string Title,
    [property: JsonPropertyName("year")] int Year,
    [property: JsonPropertyName("genres")] IReadOnlyList<string> Genres,
    [property: JsonPropertyName("keywords")] IReadOnlyList<string> Keywords,
    [property: JsonPropertyName("people")] IReadOnlyList<string> People,
    [property: JsonPropertyName("language")] string Language,
    [property: JsonPropertyName("country")] string Country,
    [property: JsonPropertyName("runtime_minutes")] int RuntimeMinutes,
    [property: JsonPropertyName("present")] bool Present);

public sealed record Activity(
    [property: JsonPropertyName("event_id")] string EventId,
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("user_id")] string UserId,
    [property: JsonPropertyName("item_id")] string ItemId,
    [property: JsonPropertyName("event_type")] string EventType,
    [property: JsonPropertyName("progress")] double Progress,
    [property: JsonPropertyName("occurred_at")] DateTime OccurredAt);

public sealed record ActivityRequest([property: JsonPropertyName("events")] IReadOnlyList<Activity> Events);

public sealed class RecommendationPage
{
    [JsonPropertyName("items")]
    public List<Recommendation> Items { get; init; } = new();
}

public sealed class Recommendation
{
    [JsonPropertyName("id")]
    public long Id { get; init; }

    [JsonPropertyName("media_type")]
    public string MediaType { get; init; } = string.Empty;

    [JsonPropertyName("tmdb_id")]
    public int TmdbId { get; init; }

    [JsonPropertyName("title")]
    public string Title { get; init; } = string.Empty;

    [JsonPropertyName("year")]
    public int Year { get; init; }
}

public sealed record RecommendationAction(
    [property: JsonPropertyName("action_id")] string ActionId,
    [property: JsonPropertyName("action")] string Action,
    [property: JsonPropertyName("rating")] int? Rating = null);

public sealed class TrialPage
{
    [JsonPropertyName("items")]
    public List<SeriesTrial> Items { get; init; } = new();
}

public sealed record SeriesTrial(
    [property: JsonPropertyName("request_id")] long RequestId,
    [property: JsonPropertyName("tmdb_id")] int TmdbId,
    [property: JsonPropertyName("episodes")] IReadOnlyList<TrialEpisode> Episodes);

public sealed record TrialEpisode(
    [property: JsonPropertyName("season")] int Season,
    [property: JsonPropertyName("number")] int Number,
    [property: JsonPropertyName("watched")] bool Watched);

public sealed record TrialPlayback(
    [property: JsonPropertyName("request_id")] long RequestId,
    [property: JsonPropertyName("season")] int Season,
    [property: JsonPropertyName("number")] int Number);

public sealed class EpisodeConfirmationPage
{
    [JsonPropertyName("items")]
    public List<PendingEpisodeConfirmation> Items { get; init; } = new();
}

public sealed record PendingEpisodeConfirmation(
    [property: JsonPropertyName("request_id")] long RequestId,
    [property: JsonPropertyName("tmdb_id")] int TmdbId,
    [property: JsonPropertyName("season")] int Season,
    [property: JsonPropertyName("number")] int Number,
    [property: JsonPropertyName("recheck")] bool Recheck);

public sealed record EpisodeKey(
    [property: JsonPropertyName("request_id")] long RequestId,
    [property: JsonPropertyName("season")] int Season,
    [property: JsonPropertyName("number")] int Number);

public sealed record EpisodeConfirmation(
    [property: JsonPropertyName("request_id")] long RequestId,
    [property: JsonPropertyName("season")] int Season,
    [property: JsonPropertyName("number")] int Number,
    [property: JsonPropertyName("item_id")] string ItemId);

public sealed record SubtitleCoverage(
    [property: JsonPropertyName("media_source_id")] string MediaSourceId,
    [property: JsonPropertyName("item_id")] string ItemId,
    [property: JsonPropertyName("media_type")] string MediaType,
    [property: JsonPropertyName("library")] string Library,
    [property: JsonPropertyName("title")] string Title,
    [property: JsonPropertyName("series_title")] string SeriesTitle,
    [property: JsonPropertyName("season")] int? Season,
    [property: JsonPropertyName("episode")] int? Episode,
    [property: JsonPropertyName("year")] int Year,
    [property: JsonPropertyName("edition")] string Edition,
    [property: JsonPropertyName("status")] string Status,
    [property: JsonPropertyName("subtitle_languages")] IReadOnlyList<string> SubtitleLanguages,
    [property: JsonPropertyName("fingerprint")] string Fingerprint);

public sealed record SubtitleLibraryInfo(
    [property: JsonPropertyName("name")] string Name,
    [property: JsonPropertyName("download_languages")] IReadOnlyList<string> DownloadLanguages,
    [property: JsonPropertyName("save_with_media")] bool SaveWithMedia,
    [property: JsonPropertyName("require_perfect_match")] bool RequirePerfectMatch);

// Providers and libraries are sent only with the completing batch.
public sealed record SubtitleCoverageRequest(
    [property: JsonPropertyName("audit_library")] string AuditLibrary,
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("sync_token")] string SyncToken,
    [property: JsonPropertyName("complete")] bool Complete,
    [property: JsonPropertyName("items")] IReadOnlyList<SubtitleCoverage> Items,
    [property: JsonPropertyName("providers"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] IReadOnlyList<string>? Providers,
    [property: JsonPropertyName("libraries"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] IReadOnlyList<SubtitleLibraryInfo>? Libraries,
    // Set only on a completing request for a pass that could not finish.
    [property: JsonPropertyName("audit_error"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] string? AuditError = null);

public sealed class SubtitleLease
{
    [JsonPropertyName("jobs")]
    public List<SubtitleJob> Jobs { get; init; } = new();

    [JsonPropertyName("audit_requested")]
    public bool AuditRequested { get; init; }

    [JsonPropertyName("audit_library")]
    public string AuditLibrary { get; init; } = string.Empty;
}

public sealed record SubtitleJob(
    [property: JsonPropertyName("lease_token")] string LeaseToken,
    [property: JsonPropertyName("id")] long Id,
    [property: JsonPropertyName("kind")] string Kind,
    [property: JsonPropertyName("media_source_id")] string MediaSourceId,
    [property: JsonPropertyName("item_id")] string ItemId,
    [property: JsonPropertyName("language")] string Language,
    [property: JsonPropertyName("fingerprint")] string Fingerprint,
    [property: JsonPropertyName("subtitle_id")] string SubtitleId);

public sealed record SubtitleCandidate(
    [property: JsonPropertyName("id")] string Id,
    [property: JsonPropertyName("provider")] string Provider,
    [property: JsonPropertyName("name")] string Name,
    [property: JsonPropertyName("language")] string Language,
    [property: JsonPropertyName("format")] string Format,
    [property: JsonPropertyName("hash_match")] bool HashMatch,
    [property: JsonPropertyName("forced")] bool Forced,
    [property: JsonPropertyName("hearing_impaired")] bool HearingImpaired,
    [property: JsonPropertyName("machine_translated")] bool MachineTranslated,
    [property: JsonPropertyName("downloads")] int Downloads);

public sealed record SubtitleJobResult(
    [property: JsonPropertyName("lease_token")] string LeaseToken,
    [property: JsonPropertyName("job_id")] long JobId,
    [property: JsonPropertyName("outcome")] string Outcome,
    [property: JsonPropertyName("detail")] string Detail,
    [property: JsonPropertyName("provider")] string Provider,
    [property: JsonPropertyName("candidates")] IReadOnlyList<SubtitleCandidate>? Candidates,
    [property: JsonPropertyName("coverage")] SubtitleCoverage? Coverage);

public sealed record SubtitleResultsRequest(
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("results")] IReadOnlyList<SubtitleJobResult> Results,
    [property: JsonPropertyName("pause_seconds")] int PauseSeconds);

public sealed record EpisodeConfirmationRequest(
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("user_id")] string UserId,
    [property: JsonPropertyName("episodes")] IReadOnlyList<EpisodeConfirmation> Episodes,
    [property: JsonPropertyName("missing")] IReadOnlyList<EpisodeKey> Missing);
