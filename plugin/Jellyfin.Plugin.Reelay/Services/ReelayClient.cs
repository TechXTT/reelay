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

    public async Task SyncAsync(SyncRequest request, CancellationToken cancellationToken)
    {
        await PostAsync("/api/v1/integrations/jellyfin/sync", request, cancellationToken).ConfigureAwait(false);
    }

    public async Task SendActivitiesAsync(IReadOnlyList<Activity> events, CancellationToken cancellationToken)
    {
        if (events.Count == 0) return;
        await PostAsync("/api/v1/integrations/jellyfin/events", new ActivityRequest(events), cancellationToken).ConfigureAwait(false);
    }

    public async Task<IReadOnlyList<Recommendation>> GetRecommendationsAsync(string userId, string mediaType, CancellationToken cancellationToken)
    {
        var config = RequiredConfiguration();
        using var request = CreateRequest(HttpMethod.Get, $"/api/v1/recommendations?server_id={Uri.EscapeDataString(config.ServerId)}&user_id={Uri.EscapeDataString(userId)}&media_type={mediaType}&limit={config.RecommendationLimit}");
        using var response = await _http.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
        var page = await response.Content.ReadFromJsonAsync<RecommendationPage>(cancellationToken: cancellationToken).ConfigureAwait(false);
        return page?.Items ?? new List<Recommendation>();
    }

    public async Task GenerateAsync(string userId, string mediaType, CancellationToken cancellationToken)
    {
        var config = RequiredConfiguration();
        await PostAsync("/api/v1/recommendations/generate", new { server_id = config.ServerId, user_id = userId, media_type = mediaType }, cancellationToken).ConfigureAwait(false);
    }

    public async Task ActAsync(long recommendationId, string actionId, string action, CancellationToken cancellationToken)
    {
        await PostAsync($"/api/v1/recommendations/{recommendationId}/actions", new RecommendationAction(actionId, action), cancellationToken).ConfigureAwait(false);
    }

    public async Task TestAsync(string url, string token, CancellationToken cancellationToken)
    {
        if (!Uri.TryCreate(url, UriKind.Absolute, out var baseUri) || (baseUri.Scheme != Uri.UriSchemeHttp && baseUri.Scheme != Uri.UriSchemeHttps))
            throw new ArgumentException("Reelay URL must be an absolute HTTP or HTTPS URL", nameof(url));
        using var request = new HttpRequestMessage(HttpMethod.Get, new Uri(url.TrimEnd('/') + "/api/v1/health"));
        if (!string.IsNullOrWhiteSpace(token)) request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token.Trim());
        request.Headers.UserAgent.ParseAdd("Jellyfin.Plugin.Reelay/0.1");
        using var response = await _http.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await EnsureSuccessAsync(response, cancellationToken).ConfigureAwait(false);
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
        var request = new HttpRequestMessage(method, config.ReelayUrl.TrimEnd('/') + path);
        if (!string.IsNullOrWhiteSpace(config.AuthToken)) request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", config.AuthToken.Trim());
        request.Headers.UserAgent.ParseAdd("Jellyfin.Plugin.Reelay/0.1");
        return request;
    }
}
