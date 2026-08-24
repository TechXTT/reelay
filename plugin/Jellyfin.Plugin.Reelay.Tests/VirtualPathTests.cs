using Jellyfin.Plugin.Reelay.Models;
using Jellyfin.Plugin.Reelay.Services;
using Xunit;

namespace Jellyfin.Plugin.Reelay.Tests;

public sealed class VirtualPathTests
{
    [Fact]
    public void MovieUsesTmdbProviderTagAndStaysBelowRoot()
    {
        var root = Path.Combine(Path.GetTempPath(), "reelay-test");
        var path = VirtualPath.Movie(root, new Recommendation { TmdbId = 329865, Title = "Arrival", Year = 2016 });
        Assert.StartsWith(root, path, StringComparison.OrdinalIgnoreCase);
        Assert.EndsWith("Arrival (2016) [tmdbid-329865].strm", path, StringComparison.Ordinal);
    }

    [Fact]
    public void SeriesCreatesOneMetadataStubInSeasonOne()
    {
        var root = Path.Combine(Path.GetTempPath(), "reelay-test");
        var path = VirtualPath.Series(root, new Recommendation { TmdbId = 1396, Title = "Breaking Bad", Year = 2008 });
        Assert.Contains(Path.Combine("Breaking Bad (2008) [tmdbid-1396]", "Season 01"), path, StringComparison.Ordinal);
        Assert.EndsWith("Breaking Bad - S01E01 [tmdbid-1396].strm", path, StringComparison.Ordinal);
    }

    [Fact]
    public void SanitizerCapsAndRemovesIllegalSuffix()
    {
        var value = VirtualPath.Sanitize(new string('x', 130) + ".");
        Assert.Equal(120, value.Length);
        Assert.False(value.EndsWith(".", StringComparison.Ordinal));
    }

    [Fact]
    public void ManagedPathUsesPathBoundariesInsteadOfSubstrings()
    {
        var root = Path.Combine(Path.GetTempPath(), "reelay-virtual");
        Assert.True(VirtualLibraryManager.IsInside(root, Path.Combine(root, "user", "movies", "item.strm")));
        Assert.False(VirtualLibraryManager.IsInside(root, root + "-backup"));
        Assert.False(VirtualLibraryManager.IsInside(root, Path.Combine(root, "..", "library", "reelay-virtual-name.mkv")));
    }
}
