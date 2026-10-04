using System.Net.Http.Headers;
using System.Net.Http.Json;
using Jellyfin.Plugin.Reelay.Models;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed class ReelayClient
{
    private readonly HttpClient _http;

    public ReelayClient(HttpClient http)
    {
        _http = http;
        _http.Timeout = TimeSpan.FromSeconds(30);
    }

    public Task SyncAsync(SyncRequest request, CancellationToken cancellationToken)
        => PostAsync("/api/v1/integrations/jellyfin/sync", request, cancellationToken);

    public async Task SendActivitiesAsync(IReadOnlyList<Activity> events, CancellationToken cancellationToken)
    {
        if (events.Count == 0) return;
        await PostAsync("/api/v1/integrations/jellyfin/events", new ActivityRequest(events), cancellationToken).ConfigureAwait(false);
    }

    public async Task<IReadOnlyList<Recommendation>> GetRecommendationsAsync(string userId, string mediaType, CancellationToken cancellationToken)
    {
        var config = RequiredConfiguration();
        var page = await GetAsync<RecommendationPage>($"/api/v1/recommendations?{UserQuery(config, userId)}&media_type={mediaType}&limit={config.RecommendationLimit}", false, cancellationToken).ConfigureAwait(false);
        return page?.Items ?? new List<Recommendation>();
    }

    public Task GenerateAsync(string userId, string mediaType, CancellationToken cancellationToken)
        => PostAsync("/api/v1/recommendations/generate", new { server_id = RequiredConfiguration().ServerId, user_id = userId, media_type = mediaType }, cancellationToken);

    public async Task<IReadOnlyList<SeriesTrial>> GetTrialsAsync(string userId, CancellationToken cancellationToken)
    {
        var page = await GetAsync<TrialPage>($"/api/v1/trials?{UserQuery(RequiredConfiguration(), userId)}", true, cancellationToken).ConfigureAwait(false);
        return page?.Items ?? new List<SeriesTrial>();
    }

    public async Task SendTrialPlaybackAsync(string userId, IReadOnlyList<TrialPlayback> episodes, CancellationToken cancellationToken)
    {
        if (episodes.Count == 0) return;
        await PostAsync("/api/v1/integrations/jellyfin/trial-playback", new { server_id = RequiredConfiguration().ServerId, user_id = userId, episodes }, cancellationToken).ConfigureAwait(false);
    }

    public Task ActAsync(long recommendationId, string actionId, string action, int? rating, CancellationToken cancellationToken)
        => PostAsync($"/api/v1/recommendations/{recommendationId}/actions", new RecommendationAction(actionId, action, rating), cancellationToken);

    public async Task TestAsync(string url, string token, CancellationToken cancellationToken)
    {
        if (!Uri.TryCreate(url, UriKind.Absolute, out var baseUri) || (baseUri.Scheme != Uri.UriSchemeHttp && baseUri.Scheme != Uri.UriSchemeHttps))
            throw new ArgumentException("Reelay URL must be an absolute HTTP or HTTPS URL", nameof(url));
        using var request = Prepare(new HttpRequestMessage(HttpMethod.Get, new Uri(url.TrimEnd('/') + "/api/v1/health")), token);
        using var response = await _http.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    private static string UserQuery(Jellyfin.Plugin.Reelay.Configuration.PluginConfiguration config, string userId)
        => $"server_id={Uri.EscapeDataString(config.ServerId)}&user_id={Uri.EscapeDataString(userId)}";

    private async Task<T?> GetAsync<T>(string path, bool missingIsEmpty, CancellationToken cancellationToken)
        where T : class
    {
        using var request = CreateRequest(HttpMethod.Get, path);
        using var response = await _http.SendAsync(request, cancellationToken).ConfigureAwait(false);
        if (missingIsEmpty && response.StatusCode == System.Net.HttpStatusCode.NotFound) return null;
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
        return await response.Content.ReadFromJsonAsync<T>(cancellationToken: cancellationToken).ConfigureAwait(false);
    }

    private async Task PostAsync<T>(string path, T body, CancellationToken cancellationToken)
    {
        using var request = CreateRequest(HttpMethod.Post, path);
        request.Content = JsonContent.Create(body);
        using var response = await _http.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
    }

    private static async Task EnsureSuccessAsync(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        if (response.IsSuccessStatusCode) return;
        var detail = await response.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
        throw new HttpRequestException($"Reelay returned {(int)response.StatusCode}: {detail}");
    }

    private static Jellyfin.Plugin.Reelay.Configuration.PluginConfiguration RequiredConfiguration()
    {
        var config = Plugin.Instance?.Configuration ?? throw new InvalidOperationException("Reelay plugin is not initialized");
        if (!Uri.TryCreate(config.ReelayUrl, UriKind.Absolute, out _)) throw new InvalidOperationException("Reelay URL is not configured");
        return config;
    }

    private static HttpRequestMessage CreateRequest(HttpMethod method, string path)
    {
        var config = RequiredConfiguration();
        return Prepare(new HttpRequestMessage(method, config.ReelayUrl.TrimEnd('/') + path), config.AuthToken);
    }

    private static HttpRequestMessage Prepare(HttpRequestMessage request, string token)
    {
        if (!string.IsNullOrWhiteSpace(token)) request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token.Trim());
        request.Headers.UserAgent.ParseAdd("Jellyfin.Plugin.Reelay/0.1");
        return request;
    }
}
