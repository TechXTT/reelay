using Jellyfin.Plugin.Reelay.Services;
using MediaBrowser.Controller;
using MediaBrowser.Controller.Plugins;
using Microsoft.Extensions.DependencyInjection;

namespace Jellyfin.Plugin.Reelay;

public sealed class PluginServiceRegistrator : IPluginServiceRegistrator
{
    public void RegisterServices(IServiceCollection serviceCollection, IServerApplicationHost applicationHost)
    {
        serviceCollection.AddHttpClient<ReelayClient>();
        serviceCollection.AddSingleton<VirtualLibraryManager>();
        serviceCollection.AddSingleton<SyncService>();
        serviceCollection.AddSingleton<ActionOutbox>();
        serviceCollection.AddHostedService<ActionMonitor>();
        serviceCollection.AddHostedService<PlaybackGuard>();
    }
}
