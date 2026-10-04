using Jellyfin.Data.Enums;
using Jellyfin.Plugin.Reelay.Configuration;
using Jellyfin.Plugin.Reelay.Models;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Entities;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using JellyfinUser = Jellyfin.Database.Implementations.Entities.User;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed class ActionMonitor : BackgroundService
{
    private readonly ILibraryManager _libraryManager;
    private readonly IUserManager _userManager;
    private readonly IUserDataManager _userDataManager;
    private readonly ReelayClient _client;
    private readonly VirtualLibraryManager _virtual;
    private readonly ActionOutbox _outbox;
    private readonly SyncService _sync;
    private readonly ILogger<ActionMonitor> _logger;

    public ActionMonitor(ILibraryManager libraryManager, IUserManager userManager, IUserDataManager userDataManager, ReelayClient client, VirtualLibraryManager virtualLibrary, ActionOutbox outbox, SyncService sync, ILogger<ActionMonitor> logger)
    {
        _libraryManager = libraryManager;
        _userManager = userManager;
        _userDataManager = userDataManager;
        _client = client;
        _virtual = virtualLibrary;
        _outbox = outbox;
        _sync = sync;
        _logger = logger;
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        await Task.Delay(TimeSpan.FromSeconds(20), stoppingToken).ConfigureAwait(false);
        using var timer = new PeriodicTimer(TimeSpan.FromMinutes(1));
        do
        {
            try { await CheckAsync(stoppingToken).ConfigureAwait(false); }
            catch (OperationCanceledException) when (stoppingToken.IsCancellationRequested) { return; }
            catch (Exception ex) { _logger.LogWarning(ex, "Could not process Reelay recommendation actions; they will be retried"); }
        }
        while (await timer.WaitForNextTickAsync(stoppingToken).ConfigureAwait(false));
    }

    private async Task CheckAsync(CancellationToken cancellationToken)
    {
        var config = Plugin.Instance?.Configuration ?? throw new InvalidOperationException("Plugin is not initialized");
        if (!config.Enabled) return;
        foreach (var action in _outbox.Snapshot()) await SendAsync(action, cancellationToken).ConfigureAwait(false);
        var virtualItems = JellyfinIdentity.Scan(_libraryManager, BaseItemKind.Movie, BaseItemKind.Series)
            .Where(item => _virtual.IsManagedPath(item.Path)).ToList();
        foreach (var user in JellyfinIdentity.EnabledUsers(_userManager, config))
        {
            await _sync.SyncTrialProgressAsync(user, cancellationToken).ConfigureAwait(false);
            var userId = user.Id.ToString("N");
            var userItems = virtualItems.Where(item => _virtual.IsUserPath(item.Path, userId)).ToList();
            await ProcessUserAsync(config, user, userId, "movie", userItems.OfType<MediaBrowser.Controller.Entities.Movies.Movie>(), cancellationToken).ConfigureAwait(false);
            await ProcessUserAsync(config, user, userId, "series", userItems.OfType<MediaBrowser.Controller.Entities.TV.Series>(), cancellationToken).ConfigureAwait(false);
        }
    }

    private async Task ProcessUserAsync(PluginConfiguration config, JellyfinUser user, string userId, string mediaType, IEnumerable<BaseItem> items, CancellationToken cancellationToken)
    {
        var recommendations = await _client.GetRecommendationsAsync(userId, mediaType, cancellationToken).ConfigureAwait(false);
        var byTmdb = recommendations.ToDictionary(item => item.TmdbId);
        var changed = false;
        foreach (var item in items)
        {
            var tmdb = JellyfinIdentity.ProviderId(item, MetadataProvider.Tmdb);
            if (tmdb == 0 || !byTmdb.TryGetValue(tmdb, out var recommendation)) continue;
            var data = _userDataManager.GetUserData(user, item);
            if (data is null) continue;
            if (ChooseAction(data.IsFavorite, data.Rating, data.Likes) is not { } choice) continue;
            var (action, rating) = choice;
            var actionId = JellyfinIdentity.StableId($"{config.ServerId}:{userId}:{recommendation.Id}:{action}:{rating}");
            var pending = new PendingAction(recommendation.Id, actionId, action, rating);
            _outbox.Enqueue(pending);
            await SendAsync(pending, cancellationToken).ConfigureAwait(false);
            changed = true;
            _logger.LogInformation("Sent {Action} for {Title} on behalf of Jellyfin user {User}", action, recommendation.Title, user.Username);
        }
        var remaining = changed
            ? await _client.GetRecommendationsAsync(userId, mediaType, cancellationToken).ConfigureAwait(false)
            : recommendations;
        _virtual.Refresh(userId, mediaType, remaining);
    }

    private static (string Action, int? Rating)? ChooseAction(bool isFavorite, double? userRating, bool? likes)
    {
        var rating = userRating is > 0 ? Math.Clamp((int)Math.Ceiling(userRating.Value / 2d), 1, 5) : (int?)null;
        if (isFavorite) return ("request", rating);
        if (rating.HasValue) return ("rate", rating);
        if (likes == false) return ("dismiss", null);
        return null;
    }

    private async Task SendAsync(PendingAction action, CancellationToken cancellationToken)
    {
        await _client.ActAsync(action.RecommendationId, action.ActionId, action.Action, action.Rating, cancellationToken).ConfigureAwait(false);
        _outbox.Complete(action.ActionId);
    }
}
