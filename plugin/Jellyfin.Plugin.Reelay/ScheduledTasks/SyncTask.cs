using Jellyfin.Plugin.Reelay.Services;
using MediaBrowser.Model.Tasks;

namespace Jellyfin.Plugin.Reelay.ScheduledTasks;

public sealed class SyncTask : IScheduledTask
{
    private readonly SyncService _sync;

    public SyncTask(SyncService sync)
    {
        _sync = sync;
    }

    public string Name => "Sync Reelay Recommendations";

    public string Key => "ReelayRecommendationSync";

    public string Description => "Synchronizes Jellyfin activity and rebuilds per-user Reelay Discover libraries.";

    public string Category => "Reelay";

    public Task ExecuteAsync(IProgress<double> progress, CancellationToken cancellationToken) => _sync.RunAsync(progress, cancellationToken);

    public IEnumerable<TaskTriggerInfo> GetDefaultTriggers()
    {
        yield return new TaskTriggerInfo { Type = TaskTriggerInfoType.StartupTrigger };
        yield return new TaskTriggerInfo { Type = TaskTriggerInfoType.IntervalTrigger, IntervalTicks = TimeSpan.FromHours(6).Ticks };
    }
}
