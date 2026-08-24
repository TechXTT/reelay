using MediaBrowser.Model.Plugins;

namespace Jellyfin.Plugin.Reelay.Configuration;

public sealed class PluginConfiguration : BasePluginConfiguration
{
    public bool Enabled { get; set; }

    public string ReelayUrl { get; set; } = "http://127.0.0.1:7878";

    public string AuthToken { get; set; } = string.Empty;

    public string ServerId { get; set; } = Guid.NewGuid().ToString("N");

    public string VirtualRoot { get; set; } = string.Empty;

    public string[] EnabledUserIds { get; set; } = Array.Empty<string>();

    public int RecommendationLimit { get; set; } = 40;
}
