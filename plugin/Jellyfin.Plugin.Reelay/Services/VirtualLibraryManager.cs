using System.Text;
using Jellyfin.Plugin.Reelay.Models;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed class VirtualLibraryManager
{
    public const string RootFolderName = "reelay-virtual";
    private const string Marker = ".reelay-managed";
    private readonly ILogger<VirtualLibraryManager> _logger;

    public VirtualLibraryManager(ILogger<VirtualLibraryManager> logger)
    {
        _logger = logger;
    }

    public string GetPath(string userId, string mediaType)
    {
        var root = Plugin.Instance?.Configuration.VirtualRoot ?? throw new InvalidOperationException("Plugin is not initialized");
        return Path.Combine(root, userId, mediaType == "movie" ? "movies" : "series");
    }

    public bool IsManagedPath(string? path) => IsInside(Plugin.Instance?.Configuration.VirtualRoot, path);

    public bool IsUserPath(string? path, string userId)
        => IsInside(Path.Combine(Plugin.Instance?.Configuration.VirtualRoot ?? string.Empty, userId), path);

    public void Refresh(string userId, string mediaType, IReadOnlyList<Recommendation> recommendations)
    {
        var path = GetPath(userId, mediaType);
        EnsureManaged(path);
        var desired = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
        foreach (var item in recommendations)
        {
            var location = mediaType == "movie" ? VirtualPath.Movie(path, item) : VirtualPath.Series(path, item);
            desired.Add(location);
            Directory.CreateDirectory(Path.GetDirectoryName(location)!);
            if (!File.Exists(location)) File.WriteAllText(location, "https://example.invalid/reelay-placeholder.mp4\n", Encoding.UTF8);
        }

        foreach (var file in Directory.EnumerateFiles(path, "*.strm", SearchOption.AllDirectories))
        {
            if (!desired.Contains(file)) File.Delete(file);
        }
        foreach (var directory in Directory.EnumerateDirectories(path, "*", SearchOption.AllDirectories).OrderByDescending(static value => value.Length))
        {
            if (!Directory.EnumerateFileSystemEntries(directory).Any()) Directory.Delete(directory);
        }
        _logger.LogInformation("Refreshed {Count} {MediaType} recommendations for Jellyfin user {UserId} at {Path}", recommendations.Count, mediaType, userId, path);
    }

    private static void EnsureManaged(string path)
    {
        Directory.CreateDirectory(path);
        var marker = Path.Combine(path, Marker);
        if (!File.Exists(marker)) File.WriteAllText(marker, "Managed by Reelay. Files below this directory may be replaced.\n");
    }

    internal static bool IsInside(string? root, string? path)
    {
        if (string.IsNullOrWhiteSpace(root) || string.IsNullOrWhiteSpace(path)) return false;
        try
        {
            var relative = Path.GetRelativePath(Path.GetFullPath(root), Path.GetFullPath(path));
            return relative != ".." && !relative.StartsWith(".." + Path.DirectorySeparatorChar, StringComparison.Ordinal);
        }
        catch (Exception)
        {
            return false;
        }
    }

}
