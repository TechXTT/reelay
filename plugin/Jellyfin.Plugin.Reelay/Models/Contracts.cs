using System.Text.Json.Serialization;

namespace Jellyfin.Plugin.Reelay.Models;

public sealed record SyncRequest(
    [property: JsonPropertyName("server_id")] string ServerId,
    [property: JsonPropertyName("sync_token")] string SyncToken,
    [property: JsonPropertyName("complete")] bool Complete,
    [property: JsonPropertyName("users")] IReadOnlyList<SyncUser> Users,
    [property: JsonPropertyName("items")] IReadOnlyList<SyncItem> Items);

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
    [property: JsonPropertyName("action")] string Action);
