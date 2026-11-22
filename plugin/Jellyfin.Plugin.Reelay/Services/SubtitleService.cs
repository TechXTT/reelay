using System.Globalization;
using System.Net;
using Jellyfin.Data.Enums;
using Jellyfin.Plugin.Reelay.Models;
using MediaBrowser.Common.Extensions;
using MediaBrowser.Common.Configuration;
using MediaBrowser.Controller.Configuration;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Entities.Movies;
using MediaBrowser.Controller.Entities.TV;
using MediaBrowser.Controller.Library;
using MediaBrowser.Controller.Providers;
using MediaBrowser.Controller.Subtitles;
using MediaBrowser.Model.Dto;
using MediaBrowser.Model.Entities;
using MediaBrowser.Model.Globalization;
using MediaBrowser.Model.IO;
using MediaBrowser.Model.MediaInfo;
using MediaBrowser.Model.Providers;
using MediaBrowser.Model.Tasks;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.Reelay.Services;

// Reads subtitle coverage from Jellyfin's probed streams and downloads
// subtitles that Reelay queued, through the providers and storage configured
// in Jellyfin. Coverage follows the zero-track rule: any subtitle stream,
// including forced-only and unknown-language tracks, counts as present.
public sealed class SubtitleService : BackgroundService
{
    private const int BatchSize = 400;
    private const int CandidateLimit = 10;

    private readonly ILibraryManager _libraryManager;
    private readonly ISubtitleManager _subtitleManager;
    private readonly IProviderManager _providerManager;
    private readonly IFileSystem _fileSystem;
    private readonly ILocalizationManager _localization;
    private readonly ReelayClient _client;
    private readonly VirtualLibraryManager _virtual;
    private readonly ILogger<SubtitleService> _logger;
    private readonly ITaskManager _tasks;
    private readonly IServerConfigurationManager _serverConfig;
    private int _auditRequested;

    public SubtitleService(ILibraryManager libraryManager, ISubtitleManager subtitleManager, IProviderManager providerManager, IFileSystem fileSystem, ILocalizationManager localization, ITaskManager tasks, IServerConfigurationManager serverConfig, ReelayClient client, VirtualLibraryManager virtualLibrary, ILogger<SubtitleService> logger)
    {
        _libraryManager = libraryManager;
        _subtitleManager = subtitleManager;
        _providerManager = providerManager;
        _fileSystem = fileSystem;
        _localization = localization;
        _tasks = tasks;
        _serverConfig = serverConfig;
        _client = client;
        _virtual = virtualLibrary;
        _logger = logger;
    }

    public void RequestAudit() => Interlocked.Exchange(ref _auditRequested, 1);

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        await Task.Delay(TimeSpan.FromSeconds(20), stoppingToken).ConfigureAwait(false);
        using var timer = new PeriodicTimer(TimeSpan.FromMinutes(1));
        do
        {
            var config = Plugin.Instance?.Configuration;
            if (config?.Enabled != true) continue;
            try
            {
                var lease = await _client.LeaseSubtitleJobsAsync(config.ServerId, stoppingToken).ConfigureAwait(false);
                if (lease is null) continue;
                foreach (var job in lease.Jobs)
                {
                    var (result, pause) = await RunJobAsync(job, stoppingToken).ConfigureAwait(false);
                    await _client.SendSubtitleResultsAsync(new SubtitleResultsRequest(config.ServerId, new[] { result }, pause), stoppingToken).ConfigureAwait(false);
                    if (pause > 0) break;
                }
                if (lease.AuditRequested && !string.IsNullOrEmpty(lease.AuditLibrary))
                {
                    await AuditAsync(config.ServerId, lease.AuditLibrary, stoppingToken).ConfigureAwait(false);
                }
                else if (Interlocked.Exchange(ref _auditRequested, 0) != 0 || lease.AuditRequested)
                {
                    if (!await AuditAsync(config.ServerId, string.Empty, stoppingToken).ConfigureAwait(false)) RequestAudit();
                }
            }
            catch (Exception ex) when (!stoppingToken.IsCancellationRequested)
            {
                _logger.LogWarning(ex, "Could not process Reelay subtitle work");
            }
        }
        while (await timer.WaitForNextTickAsync(stoppingToken).ConfigureAwait(false));
    }

    // Runs an audit and reports a failure to Reelay, so the operator sees why
    // coverage is not current instead of an audit that silently never finishes.
    private async Task<bool> AuditAsync(string serverId, string auditLibrary, CancellationToken cancellationToken)
    {
        try
        {
            await AuditCoreAsync(serverId, auditLibrary, cancellationToken).ConfigureAwait(false);
            return true;
        }
        catch (Exception ex) when (!cancellationToken.IsCancellationRequested)
        {
            _logger.LogWarning(ex, "Could not complete the Reelay subtitle audit");
            var failure = new SubtitleCoverageRequest(auditLibrary, serverId, Guid.NewGuid().ToString("N"), true, Array.Empty<SubtitleCoverage>(), null, null, ex.Message);
            await _client.SendSubtitleCoverageAsync(failure, cancellationToken).ConfigureAwait(false);
            return false;
        }
    }

    private async Task AuditCoreAsync(string serverId, string auditLibrary, CancellationToken cancellationToken)
    {
        var syncToken = Guid.NewGuid().ToString("N");
        var folders = _libraryManager.GetVirtualFolders()
            .Where(folder => folder.Locations.Length > 0 && !folder.Locations.All(_virtual.IsManagedPath))
            .ToList();
        var query = new InternalItemsQuery { IncludeItemTypes = new[] { BaseItemKind.Movie, BaseItemKind.Episode }, Recursive = true, IsVirtualItem = false };
        var auditedFolders = folders.Where(folder => auditLibrary.Length == 0 || folder.Name == auditLibrary).ToList();
        if (auditLibrary.Length != 0)
        {
            if (auditedFolders.Count != 1 || !Guid.TryParse(auditedFolders[0].ItemId, out var id))
                throw new InvalidOperationException($"Could not identify subtitle audit library '{auditLibrary}'");
            query.ParentId = id;
        }
        foreach (var location in auditedFolders.SelectMany(folder => folder.Locations).Where(path => !_virtual.IsManagedPath(path)))
        {
            // An offline mount must not turn a temporary outage into a completed empty snapshot.
            using var entries = Directory.EnumerateFileSystemEntries(location).GetEnumerator();
            entries.MoveNext();
        }
        var videos = _libraryManager.GetItemList(query)
            .OfType<Video>()
            .Where(video => !_virtual.IsManagedPath(video.Path));
        var sources = new Dictionary<string, SubtitleCoverage>(StringComparer.Ordinal);
        foreach (var video in videos)
        {
            cancellationToken.ThrowIfCancellationRequested();
            var mediaSources = video.GetMediaSources(false);
            foreach (var source in mediaSources)
            {
                // Alternate versions list each other's sources; keep the first report of each.
                if (sources.ContainsKey(source.Id) || !Guid.TryParse(source.Id, out var id)) continue;
                var owner = id == video.Id ? video : _libraryManager.GetItemById(id) as Video;
                if (owner is null) continue;
                var folder = auditedFolders.FirstOrDefault(folder => folder.Locations.Any(location => IsUnder(source.Path, location)));
                if (folder is null) continue;
                var catalogItem = owner is Movie or Episode ? owner : video;
                var coverage = Coverage(catalogItem, owner, source, folder.Name, mediaSources.Count > 1 ? Path.GetFileNameWithoutExtension(source.Path) : string.Empty);
                if (coverage is not null) sources[source.Id] = coverage;
            }
        }
        var items = sources.Values.ToList();
        _logger.LogInformation("Reporting subtitle coverage for {SourceCount} Jellyfin media sources: {Missing} without subtitles, {Unknown} not probed yet",
            items.Count, items.Count(static item => item.Status == "missing"), items.Count(static item => item.Status == "unknown"));
        foreach (var chunk in items.Chunk(BatchSize))
        {
            if (!await _client.SendSubtitleCoverageAsync(new SubtitleCoverageRequest(auditLibrary, serverId, syncToken, false, chunk, null, null), cancellationToken).ConfigureAwait(false)) return;
        }
        var libraries = folders.Select(folder => new SubtitleLibraryInfo(
            folder.Name,
            EffectiveDownloadLanguages(folder.LibraryOptions?.SubtitleDownloadLanguages),
            folder.LibraryOptions?.SaveSubtitlesWithMedia ?? true,
            folder.LibraryOptions?.RequirePerfectSubtitleMatch ?? true)).ToList();
        await _client.SendSubtitleCoverageAsync(new SubtitleCoverageRequest(auditLibrary, serverId, syncToken, true, Array.Empty<SubtitleCoverage>(), ProviderNames(), libraries), cancellationToken).ConfigureAwait(false);
    }

    private async Task<(SubtitleJobResult Result, int PauseSeconds)> RunJobAsync(SubtitleJob job, CancellationToken cancellationToken)
    {
        var catalogItem = Guid.TryParse(job.ItemId, out var itemId) ? _libraryManager.GetItemById(itemId) as Video : null;
        if (catalogItem is not (Movie or Episode) || _virtual.IsManagedPath(catalogItem.Path))
            return (Result(job, "stale", "The movie or episode is no longer in Jellyfin", null), 0);

        (Video? Video, MediaSourceInfo? Source, SubtitleCoverage? Coverage) Inspect()
        {
            var video = Guid.TryParse(job.MediaSourceId, out var id) ? _libraryManager.GetItemById(id) as Video : null;
            var source = catalogItem.GetMediaSources(false).FirstOrDefault(item => string.Equals(item.Id, job.MediaSourceId, StringComparison.OrdinalIgnoreCase));
            return (video, source, video is null || source is null ? null : Coverage(catalogItem, video, source, string.Empty, string.Empty));
        }

        var (video, source, coverage) = Inspect();
        if (video is null || source is null || coverage is null)
            return (Result(job, "stale", "The media file is no longer accessible in Jellyfin", null), 0);
        if (coverage.Fingerprint != job.Fingerprint)
            return (Result(job, "stale", "The file changed since the job was queued", coverage), 0);
        if (job.Kind == "verify")
        {
            if (coverage.Status == "present" && HasLanguage(source, job.Language)) return (Result(job, "confirmed", string.Empty, coverage), 0);
            try { QueueRefresh(video); }
            catch (Exception ex) { return (Result(job, "not_visible", "Jellyfin refresh failed: " + ex.Message, coverage), 0); }
            return (Result(job, "not_visible", "Jellyfin does not list the downloaded track yet", coverage), 0);
        }
        // Exclude covered and unprobed files before even contacting a provider.
        if (coverage.Status == "present") return (Result(job, "covered", "Subtitles are already present", coverage), 0);
        if (coverage.Status != "missing") return (Result(job, "stale", "Jellyfin has not probed the file yet", coverage), 0);
        var options = _libraryManager.GetLibraryOptions(video);
        if (!_subtitleManager.GetSupportedProviders(catalogItem).Any(provider =>
            !(options.DisabledSubtitleFetchers ?? Array.Empty<string>()).Contains(provider.Name, StringComparer.OrdinalIgnoreCase)))
        {
            return (Result(job, "no_provider", "No subtitle provider is enabled in Jellyfin for this item", coverage), 0);
        }
        if (ScheduleConflict(options, job.Language) is { } conflict) return (Result(job, "failed", conflict, coverage), 0);
        try
        {
            var found = await SearchAsync(catalogItem, source, job.Language, options, cancellationToken).ConfigureAwait(false);
            var matching = found.Where(item => SameLanguage(item.ThreeLetterISOLanguageName, job.Language)).ToList();
            if (matching.Count == 0) return (Result(job, "no_match", "No provider returned subtitles in this language", coverage), 0);
            var chosen = string.IsNullOrEmpty(job.SubtitleId)
                ? matching.Where(static item => item.IsHashMatch == true && item.Forced != true && item.MachineTranslated != true && item.AiTranslated != true)
                    .OrderByDescending(static item => item.DownloadCount ?? 0).FirstOrDefault()
                : matching.FirstOrDefault(item => item.Id == job.SubtitleId);
            if (chosen is null)
            {
                var candidates = matching.Take(CandidateLimit).Select(static item => new SubtitleCandidate(
                    item.Id, item.ProviderName ?? string.Empty, item.Name ?? string.Empty, item.ThreeLetterISOLanguageName ?? string.Empty, item.Format ?? string.Empty,
                    item.IsHashMatch == true, item.Forced == true, item.HearingImpaired == true, item.MachineTranslated == true || item.AiTranslated == true, item.DownloadCount ?? 0)).ToList();
                var detail = string.IsNullOrEmpty(job.SubtitleId) ? "No automatic match for this exact file" : "The selected subtitle is no longer available";
                return (Result(job, "weak_match", detail, coverage) with { Candidates = candidates }, 0);
            }
            var provider = chosen.ProviderName ?? string.Empty;
            (video, source, coverage) = Inspect();
            if (video is null || coverage is null || coverage.Fingerprint != job.Fingerprint)
                return (Result(job, "stale", "The media file changed or became inaccessible before download", coverage), 0);
            if (coverage.Status == "present") return (Result(job, "covered", "Subtitles appeared before download", coverage), 0);
            if (coverage.Status != "missing") return (Result(job, "stale", "The current file is awaiting a Jellyfin probe", coverage), 0);
            options = _libraryManager.GetLibraryOptions(video);
            if (ScheduleConflict(options, job.Language) is { } changedSchedule) return (Result(job, "failed", changedSchedule, coverage), 0);
            if ((options.DisabledSubtitleFetchers ?? Array.Empty<string>()).Contains(provider, StringComparer.OrdinalIgnoreCase))
                return (Result(job, "no_provider", "The selected subtitle provider was disabled during the search", coverage), 0);
            await _subtitleManager.DownloadSubtitles(video, options, chosen.Id, cancellationToken).ConfigureAwait(false);
            try { QueueRefresh(video); }
            catch (Exception ex)
            {
                return (Result(job, "downloaded", "Subtitle saved but Jellyfin refresh failed: " + ex.Message, coverage) with { Provider = provider }, 0);
            }
            _logger.LogInformation("Downloaded {Language} subtitles for {Item} through Jellyfin", job.Language, video.Name);
            return (Result(job, "downloaded", string.Empty, coverage) with { Provider = provider }, 0);
        }
        catch (OperationCanceledException) when (cancellationToken.IsCancellationRequested)
        {
            throw;
        }
        catch (Exception ex) when (IsRateLimit(ex))
        {
            _logger.LogWarning(ex, "Subtitle provider limit reached; pausing Reelay subtitle downloads");
            return (Result(job, "rate_limited", ex.Message, coverage), 3600);
        }
        catch (Exception ex)
        {
            _logger.LogWarning(ex, "Could not download {Language} subtitles for {Item}", job.Language, video?.Name);
            var detail = IsStorage(ex) ? "Could not save the subtitle file: " + ex.Message : ex.Message;
            return (Result(job, "failed", detail, coverage), 0);
        }
    }

    private string[] EffectiveDownloadLanguages(string[]? libraryLanguages)
    {
#if JELLYFIN_10_11
        return libraryLanguages ?? _serverConfig.GetConfiguration<SubtitleOptions>("subtitles").DownloadLanguages ?? Array.Empty<string>();
#else
        return libraryLanguages ?? Array.Empty<string>();
#endif
    }

    private string? ScheduleConflict(MediaBrowser.Model.Configuration.LibraryOptions options, string language)
    {
        var task = _tasks.ScheduledTasks.FirstOrDefault(worker => worker.ScheduledTask.Key == "DownloadSubtitles");
        if (task is null || !EffectiveDownloadLanguages(options.SubtitleDownloadLanguages).Any(value => SameLanguage(value, language))) return null;
        return task.State != TaskState.Idle || task.Triggers.Any()
            ? "Jellyfin's Download Missing Subtitles task also handles this language. Remove the overlapping language or disable its triggers and wait for any running task to finish."
            : null;
    }

    private Task<RemoteSubtitleInfo[]> SearchAsync(Video video, MediaSourceInfo source, string language, MediaBrowser.Model.Configuration.LibraryOptions options, CancellationToken cancellationToken)
    {
        var request = new SubtitleSearchRequest
        {
            ContentType = video is Episode ? VideoContentType.Episode : VideoContentType.Movie,
            IndexNumber = video.IndexNumber,
            Language = language,
            MediaPath = source.Path,
            Name = video.Name,
            ParentIndexNumber = video.ParentIndexNumber,
            ProductionYear = video.ProductionYear,
            ProviderIds = video.ProviderIds,
            RuntimeTicks = source.RunTimeTicks,
            // Every enabled provider, so a weak match can be compared with others.
            SearchAllProviders = true,
            IsPerfectMatch = false,
            DisabledSubtitleFetchers = options.DisabledSubtitleFetchers ?? Array.Empty<string>(),
            SubtitleFetcherOrder = options.SubtitleFetcherOrder ?? Array.Empty<string>(),
            IsAutomated = true
        };
        if (video is Episode episode)
        {
            request.IndexNumberEnd = episode.IndexNumberEnd;
            request.SeriesName = episode.SeriesName;
        }
        return _subtitleManager.SearchSubtitles(request, cancellationToken);
    }

    // See docs/ai_context/gotchas/jellyfin-subtitles.md - DownloadSubtitles does not refresh the item.
    private void QueueRefresh(Video video)
        => _providerManager.QueueRefresh(video.Id, new MetadataRefreshOptions(new DirectoryService(_fileSystem)), RefreshPriority.High);

    private SubtitleCoverage? Coverage(Video catalogItem, Video owner, MediaSourceInfo source, string library, string edition)
    {
        if (source.Protocol != MediaProtocol.File || source.IsRemote || string.IsNullOrEmpty(source.Path) ||
            !string.Equals(source.Path, owner.Path, StringComparison.Ordinal) || _virtual.IsManagedPath(source.Path)) return null;
        long length;
        DateTime modified;
        try
        {
            using var file = File.OpenHandle(source.Path, FileMode.Open, FileAccess.Read, FileShare.ReadWrite | FileShare.Delete);
            length = RandomAccess.GetLength(file);
            modified = File.GetLastWriteTimeUtc(file);
        }
        catch (Exception ex) when (ex is IOException or UnauthorizedAccessException) { return null; }
        var streams = source.MediaStreams ?? Array.Empty<MediaStream>();
        var subtitles = streams.Where(static stream => stream.Type == MediaStreamType.Subtitle).ToList();
        var probed = streams.Any(static stream => stream.Type is MediaStreamType.Video or MediaStreamType.Audio);
        var staleProbe = owner.HasChanged(modified) || (source.Size is > 0 && source.Size != length);
        var status = staleProbe ? "unknown" : subtitles.Count > 0 ? "present" : probed ? "missing" : "unknown";
        var languages = subtitles.Select(static stream => string.IsNullOrWhiteSpace(stream.Language) ? "und" : stream.Language.ToLowerInvariant()).Distinct().ToList();
        var episode = catalogItem as Episode;
        return new SubtitleCoverage(
            source.Id,
            catalogItem.Id.ToString("N"),
            episode is null ? "movie" : "episode",
            library,
            catalogItem.Name ?? string.Empty,
            episode?.SeriesName ?? string.Empty,
            episode?.ParentIndexNumber,
            episode?.IndexNumber,
            catalogItem.ProductionYear ?? 0,
            edition,
            status,
            staleProbe ? Array.Empty<string>() : languages,
            JellyfinIdentity.StableId(string.Join('|', source.Path, length.ToString(CultureInfo.InvariantCulture), modified.Ticks.ToString(CultureInfo.InvariantCulture))));
    }

    private bool HasLanguage(MediaSourceInfo source, string language)
        => (source.MediaStreams ?? Array.Empty<MediaStream>()).Any(stream => stream.Type == MediaStreamType.Subtitle && SameLanguage(stream.Language, language));

    // Providers and streams may name a language with a two- or three-letter
    // code (including bibliographic variants such as fre and fra).
    private bool SameLanguage(string? value, string language)
    {
        if (string.IsNullOrWhiteSpace(value)) return false;
        if (string.Equals(value, language, StringComparison.OrdinalIgnoreCase)) return true;
        var wanted = _localization.FindLanguageInfo(language);
        var actual = _localization.FindLanguageInfo(value);
        return wanted is not null && actual is not null && string.Equals(wanted.Name, actual.Name, StringComparison.OrdinalIgnoreCase);
    }

    private IReadOnlyList<string> ProviderNames()
    {
        var names = new SortedSet<string>(StringComparer.OrdinalIgnoreCase);
        foreach (var kind in new Video[] { new Movie(), new Episode() })
        {
            foreach (var provider in _subtitleManager.GetSupportedProviders(kind)) names.Add(provider.Name);
        }
        return names.ToList();
    }

    private static bool IsUnder(string? path, string folder)
        => path is not null && path.StartsWith(folder.TrimEnd('/', '\\') + Path.DirectorySeparatorChar, OperatingSystem.IsWindows() ? StringComparison.OrdinalIgnoreCase : StringComparison.Ordinal);

    private static bool IsRateLimit(Exception ex) => ex switch
    {
        RateLimitExceededException => true,
        HttpRequestException { StatusCode: HttpStatusCode.TooManyRequests } => true,
        AggregateException aggregate => aggregate.InnerExceptions.Any(IsRateLimit),
        _ => ex.InnerException is not null && IsRateLimit(ex.InnerException)
    };

    private static bool IsStorage(Exception ex) => ex switch
    {
        IOException or UnauthorizedAccessException => true,
        AggregateException aggregate => aggregate.InnerExceptions.Any(IsStorage),
        _ => false
    };

    private static SubtitleJobResult Result(SubtitleJob job, string outcome, string detail, SubtitleCoverage? coverage)
        => new(job.LeaseToken, job.Id, outcome, detail, string.Empty, null, coverage);
}
