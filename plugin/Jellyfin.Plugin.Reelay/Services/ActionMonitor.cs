using Jellyfin.Data.Enums;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Entities;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed class ActionMonitor : BackgroundService
{
    private readonly ILibraryManager _libraryManager;
    private readonly IUserManager _userManager;
    private readonly IUserDataManager _userDataManager;
    private readonly ReelayClient _client;
    private readonly VirtualLibraryManager _virtual;
    private readonly ActionOutbox _outbox;
    private readonly ILogger<ActionMonitor> _logger;

    public ActionMonitor(ILibraryManager libraryManager, IUserManager userManager, IUserDataManager userDataManager, ReelayClient client, VirtualLibraryManager virtualLibrary, ActionOutbox outbox, ILogger<ActionMonitor> logger)
    {
        _libraryManager = libraryManager;
        _userManager = userManager;
        _userDataManager = userDataManager;
        _client = client;
        _virtual = virtualLibrary;
        _outbox = outbox;
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
        await FlushOutboxAsync(cancellationToken).ConfigureAwait(false);
        var virtualItems = _libraryManager.GetItemList(new InternalItemsQuery { IncludeItemTypes = new[] { BaseItemKind.Movie, BaseItemKind.Series }, Recursive = true })
            .Where(item => _virtual.IsManagedPath(item.Path)).ToList();
        foreach (var user in JellyfinIdentity.EnabledUsers(_userManager, config))
        {
            var userId = user.Id.ToString("N");
            foreach (var mediaType in new[] { "movie", "series" })
            {
                var recommendations = await _client.GetRecommendationsAsync(userId, mediaType, cancellationToken).ConfigureAwait(false);
                var byTmdb = recommendations.ToDictionary(item => item.TmdbId);
                foreach (var item in virtualItems.Where(item => _virtual.IsUserPath(item.Path, userId)))
                {
                    var tmdb = JellyfinIdentity.ProviderId(item, MetadataProvider.Tmdb);
                    if (tmdb == 0 || !byTmdb.TryGetValue(tmdb, out var recommendation)) continue;
                    var data = _userDataManager.GetUserData(user, item);
                    if (data is null) continue;
                    var action = data.IsFavorite ? "request" : data.Likes == false ? "dismiss" : string.Empty;
                    if (action == string.Empty) continue;
                    var actionId = JellyfinIdentity.StableId($"{config.ServerId}:{userId}:{recommendation.Id}:{action}");
                    var pending = new PendingAction(recommendation.Id, actionId, action);
                    _outbox.Enqueue(pending);
                    await SendAsync(pending, cancellationToken).ConfigureAwait(false);
                    _logger.LogInformation("Sent {Action} for {Title} on behalf of Jellyfin user {User}", action, recommendation.Title, user.Username);
                }
                var remaining = await _client.GetRecommendationsAsync(userId, mediaType, cancellationToken).ConfigureAwait(false);
                _virtual.Refresh(userId, mediaType, remaining);
            }
        }
    }

    private async Task FlushOutboxAsync(CancellationToken cancellationToken)
    {
        foreach (var action in _outbox.Snapshot()) await SendAsync(action, cancellationToken).ConfigureAwait(false);
    }

    private async Task SendAsync(PendingAction action, CancellationToken cancellationToken)
    {
        await _client.ActAsync(action.RecommendationId, action.ActionId, action.Action, cancellationToken).ConfigureAwait(false);
        _outbox.Complete(action.ActionId);
    }
}
