using System.Globalization;
using Jellyfin.Data.Enums;
using Jellyfin.Plugin.Reelay.Models;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Entities;
using Microsoft.Extensions.Logging;
using JellyfinUser = Jellyfin.Database.Implementations.Entities.User;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed class SyncService
{
    private const int BatchSize = 400;

    private static readonly string[] MediaTypes = { "movie", "series" };

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
        var scanned = JellyfinIdentity.Scan(_libraryManager, BaseItemKind.Movie, BaseItemKind.Series);
        var source = scanned.Where(item => !_virtual.IsManagedPath(item.Path)).ToDictionary(item => item.Id.ToString("N"));
        var items = source.Values
            .Select(item => ToSyncItem(config.ServerId, item))
            .Where(static item => item.TmdbId > 0)
            .ToList();
        _logger.LogInformation("Found {ScannedCount} Jellyfin movies and series; synchronizing {ItemCount} real items with TMDB IDs", scanned.Count, items.Count);

        await SyncCatalogAsync(config.ServerId, users, items, now, cancellationToken).ConfigureAwait(false);
        progress?.Report(35);

        foreach (var user in users)
        {
            var events = BuildActivities(config.ServerId, user, items, source, now);
            foreach (var chunk in events.Chunk(BatchSize)) await _client.SendActivitiesAsync(chunk, cancellationToken).ConfigureAwait(false);
            await SyncTrialProgressAsync(user, cancellationToken).ConfigureAwait(false);
        }
        progress?.Report(55);

        await RefreshRecommendationsAsync(users, progress, cancellationToken).ConfigureAwait(false);
        _logger.LogInformation("Synchronized {ItemCount} Jellyfin items for {UserCount} users", items.Count, users.Count);
    }

    public async Task SyncTrialProgressAsync(JellyfinUser user, CancellationToken cancellationToken)
    {
        var userId = user.Id.ToString("N");
        var trials = await _client.GetTrialsAsync(userId, cancellationToken).ConfigureAwait(false);
        if (trials.Count == 0) return;
        var seriesByTmdb = new Dictionary<int, BaseItem>();
        foreach (var series in JellyfinIdentity.Scan(_libraryManager, BaseItemKind.Series).Where(item => !_virtual.IsManagedPath(item.Path)))
        {
            seriesByTmdb.TryAdd(JellyfinIdentity.ProviderId(series, MetadataProvider.Tmdb), series);
        }
        var watched = new List<TrialPlayback>();
        foreach (var trial in trials)
        {
            if (!seriesByTmdb.TryGetValue(trial.TmdbId, out var parent)) continue;
            foreach (var selected in trial.Episodes.Where(episode => !episode.Watched))
            {
                var episode = _libraryManager.GetItemList(new InternalItemsQuery { ParentId = parent.Id, IncludeItemTypes = new[] { BaseItemKind.Episode }, Recursive = true, ParentIndexNumber = selected.Season, IndexNumber = selected.Number, Limit = 1 }).FirstOrDefault();
                if (episode is null || _virtual.IsManagedPath(episode.Path)) continue;
                if (_userDataManager.GetUserData(user, episode)?.Played != true) continue;
                watched.Add(new TrialPlayback(trial.RequestId, selected.Season, selected.Number));
            }
        }
        foreach (var chunk in watched.Chunk(BatchSize)) await _client.SendTrialPlaybackAsync(userId, chunk, cancellationToken).ConfigureAwait(false);
    }

    private async Task SyncCatalogAsync(string serverId, IReadOnlyList<JellyfinUser> users, IReadOnlyList<SyncItem> items, DateTime now, CancellationToken cancellationToken)
    {
        var syncToken = Guid.NewGuid().ToString("N");
        var syncUsers = users.Select(user => new SyncUser(serverId, user.Id.ToString("N"), user.Username, true, now)).ToList();
        await _client.SyncAsync(new SyncRequest(serverId, syncToken, false, syncUsers, Array.Empty<SyncItem>()), cancellationToken).ConfigureAwait(false);
        foreach (var chunk in items.Chunk(BatchSize))
        {
            await _client.SyncAsync(new SyncRequest(serverId, syncToken, false, Array.Empty<SyncUser>(), chunk), cancellationToken).ConfigureAwait(false);
        }
        await _client.SyncAsync(new SyncRequest(serverId, syncToken, true, Array.Empty<SyncUser>(), Array.Empty<SyncItem>()), cancellationToken).ConfigureAwait(false);
    }

    private async Task RefreshRecommendationsAsync(IReadOnlyList<JellyfinUser> users, IProgress<double>? progress, CancellationToken cancellationToken)
    {
        var completed = 0;
        foreach (var user in users)
        {
            var userId = user.Id.ToString("N");
            foreach (var mediaType in MediaTypes)
            {
                await _client.GenerateAsync(userId, mediaType, cancellationToken).ConfigureAwait(false);
                var values = await _client.GetRecommendationsAsync(userId, mediaType, cancellationToken).ConfigureAwait(false);
                _virtual.Refresh(userId, mediaType, values);
                completed++;
                progress?.Report(55 + (45d * completed / Math.Max(1, users.Count * MediaTypes.Length)));
            }
        }
    }

    private List<Activity> BuildActivities(string serverId, JellyfinUser user, IReadOnlyList<SyncItem> items, IReadOnlyDictionary<string, BaseItem> source, DateTime now)
    {
        var userId = user.Id.ToString("N");
        var events = new List<Activity>();
        foreach (var item in items)
        {
            if (!source.TryGetValue(item.ItemId, out var entity)) continue;
            var data = _userDataManager.GetUserData(user, entity);
            if (data is null) continue;
            foreach (var (type, progress) in Signals(data.Played, data.IsFavorite, data.Likes, data.Rating))
            {
                events.Add(ActivityFor(serverId, userId, item.ItemId, type, progress, now));
            }
        }
        return events;
    }

    private static IEnumerable<(string Type, double Progress)> Signals(bool played, bool favorite, bool? likes, double? rating)
    {
        if (played) yield return ("completed", 1);
        if (favorite) yield return ("favorite", 1);
        if (likes == true) yield return ("like", 1);
        if (likes == false) yield return ("dislike", 0);
        if (rating is > 0) yield return ("rating", Math.Clamp(rating.Value / 10d, 0, 1));
    }

    private static Activity ActivityFor(string serverId, string userId, string itemId, string type, double progress, DateTime now)
    {
        var id = JellyfinIdentity.StableId($"{serverId}:{userId}:{itemId}:{type}:{progress.ToString(CultureInfo.InvariantCulture)}");
        return new Activity(id, serverId, userId, itemId, type, progress, now);
    }

    private static SyncItem ToSyncItem(string serverId, BaseItem item)
    {
        var mediaType = item is MediaBrowser.Controller.Entities.Movies.Movie ? "movie" : "series";
        return new SyncItem(serverId, item.Id.ToString("N"), mediaType, JellyfinIdentity.ProviderId(item, MetadataProvider.Tmdb), JellyfinIdentity.ProviderId(item, MetadataProvider.Tvdb), item.GetProviderId(MetadataProvider.Imdb) ?? string.Empty, item.Name, item.ProductionYear ?? 0, item.Genres, item.Tags, Array.Empty<string>(), item.PreferredMetadataLanguage ?? string.Empty, string.Empty, (int)((item.RunTimeTicks ?? 0) / TimeSpan.TicksPerMinute), true);
    }
}
