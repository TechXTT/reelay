using Jellyfin.Plugin.Reelay.Configuration;
using MediaBrowser.Common.Configuration;
using MediaBrowser.Common.Plugins;
using MediaBrowser.Model.Plugins;
using MediaBrowser.Model.Serialization;

namespace Jellyfin.Plugin.Reelay;

public sealed class Plugin : BasePlugin<PluginConfiguration>, IHasWebPages
{
    public Plugin(IApplicationPaths applicationPaths, IXmlSerializer xmlSerializer)
        : base(applicationPaths, xmlSerializer)
    {
        Instance = this;
        if (string.IsNullOrWhiteSpace(Configuration.VirtualRoot))
        {
            Configuration.VirtualRoot = Path.Combine(DataFolderPath, "reelay-virtual");
            SaveConfiguration();
        }
    }

    public static Plugin? Instance { get; private set; }

    public override string Name => "Reelay";

    public override string Description => "Per-user recommendations and acquisition through Reelay.";

    public override Guid Id => Guid.Parse("d704b447-e6e7-48df-8f45-01b42e97a741");

    public IEnumerable<PluginPageInfo> GetPages()
    {
        yield return new PluginPageInfo
        {
            Name = Name,
            EmbeddedResourcePath = GetType().Namespace + ".Configuration.configPage.html"
        };
    }
}
