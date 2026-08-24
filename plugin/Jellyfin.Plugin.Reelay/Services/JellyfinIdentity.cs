using System.Globalization;
using System.Security.Cryptography;
using System.Text;
using Jellyfin.Plugin.Reelay.Configuration;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Library;
using MediaBrowser.Model.Entities;

namespace Jellyfin.Plugin.Reelay.Services;

internal static class JellyfinIdentity
{
    public static IReadOnlyList<Jellyfin.Database.Implementations.Entities.User> EnabledUsers(IUserManager users, PluginConfiguration config)
    {
        var enabled = config.EnabledUserIds.ToHashSet(StringComparer.OrdinalIgnoreCase);
        return users.GetUsers().Where(user => enabled.Count == 0 || enabled.Contains(user.Id.ToString("N"))).ToList();
    }

    public static int ProviderId(BaseItem item, MetadataProvider provider)
        => int.TryParse(item.GetProviderId(provider), NumberStyles.None, CultureInfo.InvariantCulture, out var id) ? id : 0;

    public static string StableId(string value)
        => Convert.ToHexString(SHA256.HashData(Encoding.UTF8.GetBytes(value))).ToLowerInvariant();
}
