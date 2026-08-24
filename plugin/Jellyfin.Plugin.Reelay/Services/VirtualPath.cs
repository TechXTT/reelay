using System.Globalization;
using Jellyfin.Plugin.Reelay.Models;

namespace Jellyfin.Plugin.Reelay.Services;

internal static class VirtualPath
{
    public static string Movie(string root, Recommendation item)
    {
        var name = $"{Sanitize(item.Title)} ({item.Year.ToString(CultureInfo.InvariantCulture)}) [tmdbid-{item.TmdbId.ToString(CultureInfo.InvariantCulture)}].strm";
        return Path.Combine(root, name);
    }

    public static string Series(string root, Recommendation item)
    {
        var title = Sanitize(item.Title);
        var tmdbId = item.TmdbId.ToString(CultureInfo.InvariantCulture);
        var folder = $"{title} ({item.Year.ToString(CultureInfo.InvariantCulture)}) [tmdbid-{tmdbId}]";
        return Path.Combine(root, folder, "Season 01", $"{title} - S01E01 [tmdbid-{tmdbId}].strm");
    }

    public static string Sanitize(string value)
    {
        foreach (var invalid in Path.GetInvalidFileNameChars()) value = value.Replace(invalid, '_');
        value = value.Trim().TrimEnd('.');
        return value.Length > 120 ? value[..120] : value;
    }
}
