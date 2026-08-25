using System.Globalization;
using Jellyfin.Data.Enums;
using Jellyfin.Plugin.Reelay.Models;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Entities;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed class SyncService
{
    private readonly ILibraryManager _libraryManager;
    private readonly IUserManager _userManager;
    private readonly IUserDataManager _userDataManager;
    private readonly ReelayClient _client;
    private readonly VirtualLibraryManager _virtual;
    private readonly ILogger<SyncService> _logger;

    public SyncService(ILibraryManager libraryManager, IUserManager userManager, IUserDataManager userDataManager, ReelayClient client, VirtualLibraryManager virtualLibrary, ILogger<SyncService> logger)
    {
        _libraryManager = libraryManager;
        _userManager = userManager;
        _userDataManager = userDataManager;
        _client = client;
        _virtual = virtualLibrary;
        _logger = logger;
    }

    public async Task RunAsync(IProgress<double>? progress, CancellationToken cancellationToken)
    {
        var config = Plugin.Instance?.Configuration ?? throw new InvalidOperationException("Plugin is not initialized");
        if (!config.Enabled)
        {
            _logger.LogDebug("Reelay recommendation sync is disabled");
            progress?.Report(100);
            return;
        }
        var users = JellyfinIdentity.EnabledUsers(_userManager, config);
        var now = DateTime.UtcNow;
        var syncToken = Guid.NewGuid().ToString("N");
        var syncUsers = users.Select(user => new SyncUser(config.ServerId, user.Id.ToString("N"), user.Username, true, now)).ToList();
        var scanned = _libraryManager.GetItemList(new InternalItemsQuery { IncludeItemTypes = new[] { BaseItemKind.Movie, BaseItemKind.Series }, Recursive = true });
        var source = scanned.Where(item => !_virtual.IsManagedPath(item.Path)).ToDictionary(item => item.Id.ToString("N"));
        var items = source.Values
            .Select(item => ToSyncItem(config.ServerId, item)).Where(static item => item.TmdbId > 0).ToList();
        _logger.LogInformation("Found {ScannedCount} Jellyfin movies and series; synchronizing {ItemCount} real items with TMDB IDs", scanned.Count, items.Count);

        await _client.SyncAsync(new SyncRequest(config.ServerId, syncToken, false, syncUsers, Array.Empty<SyncItem>()), cancellationToken).ConfigureAwait(false);
        for (var offset = 0; offset < items.Count; offset += 400)
        {
            await _client.SyncAsync(new SyncRequest(config.ServerId, syncToken, false, Array.Empty<SyncUser>(), items.Skip(offset).Take(400).ToList()), cancellationToken).ConfigureAwait(false);
        }
        await _client.SyncAsync(new SyncRequest(config.ServerId, syncToken, true, Array.Empty<SyncUser>(), Array.Empty<SyncItem>()), cancellationToken).ConfigureAwait(false);
        progress?.Report(35);

        foreach (var user in users)
        {
            var events = BuildActivities(config.ServerId, user, items, source, now);
            for (var offset = 0; offset < events.Count; offset += 400) await _client.SendActivitiesAsync(events.Skip(offset).Take(400).ToList(), cancellationToken).ConfigureAwait(false);
        }
        progress?.Report(55);

        var completed = 0;
        foreach (var user in users)
        {
            foreach (var mediaType in new[] { "movie", "series" })
            {
                await _client.GenerateAsync(user.Id.ToString("N"), mediaType, cancellationToken).ConfigureAwait(false);
                var values = await _client.GetRecommendationsAsync(user.Id.ToString("N"), mediaType, cancellationToken).ConfigureAwait(false);
                _virtual.Refresh(user.Id.ToString("N"), mediaType, values);
                completed++;
                progress?.Report(55 + (45d * completed / Math.Max(1, users.Count * 2)));
            }
        }
        _logger.LogInformation("Synchronized {ItemCount} Jellyfin items for {UserCount} users", items.Count, users.Count);
    }

    private List<Activity> BuildActivities(string serverId, Jellyfin.Database.Implementations.Entities.User user, IReadOnlyList<SyncItem> items, IReadOnlyDictionary<string, BaseItem> source, DateTime now)
    {
        var events = new List<Activity>();
        foreach (var item in items)
        {
            if (!source.TryGetValue(item.ItemId, out var entity)) continue;
            var data = _userDataManager.GetUserData(user, entity);
            if (data is null) continue;
            if (data.Played) events.Add(ActivityFor(serverId, user.Id, item.ItemId, "completed", 1, now));
            if (data.IsFavorite) events.Add(ActivityFor(serverId, user.Id, item.ItemId, "favorite", 1, now));
            if (data.Likes == true) events.Add(ActivityFor(serverId, user.Id, item.ItemId, "like", 1, now));
            if (data.Likes == false) events.Add(ActivityFor(serverId, user.Id, item.ItemId, "dislike", 0, now));
            if (data.Rating is > 0) events.Add(ActivityFor(serverId, user.Id, item.ItemId, "rating", Math.Clamp(data.Rating.Value / 10d, 0, 1), now));
        }
        return events;
    }

    private static Activity ActivityFor(string serverId, Guid userId, string itemId, string type, double progress, DateTime now)
    {
        var key = $"{serverId}:{userId:N}:{itemId}:{type}:{progress.ToString(CultureInfo.InvariantCulture)}";
        var id = JellyfinIdentity.StableId(key);
        return new Activity(id, serverId, userId.ToString("N"), itemId, type, progress, now);
    }

    private static SyncItem ToSyncItem(string serverId, BaseItem item)
    {
        var mediaType = item is MediaBrowser.Controller.Entities.Movies.Movie ? "movie" : "series";
        return new SyncItem(serverId, item.Id.ToString("N"), mediaType, JellyfinIdentity.ProviderId(item, MetadataProvider.Tmdb), JellyfinIdentity.ProviderId(item, MetadataProvider.Tvdb), item.GetProviderId(MetadataProvider.Imdb) ?? string.Empty, item.Name, item.ProductionYear ?? 0, item.Genres, item.Tags, Array.Empty<string>(), item.PreferredMetadataLanguage ?? string.Empty, string.Empty, (int)((item.RunTimeTicks ?? 0) / TimeSpan.TicksPerMinute), true);
    }
}
