using System.Text;
using Xunit;

namespace Jellyfin.Plugin.Reelay.Tests;

public sealed class ConfigurationPageTests
{
    [Fact]
    public void ConfigurationScriptIsInsideJellyfinPageContainer()
    {
        const string resourceName = "Jellyfin.Plugin.Reelay.Tests.Configuration.configPage.html";
        using var stream = typeof(ConfigurationPageTests).Assembly.GetManifestResourceStream(resourceName);
        Assert.NotNull(stream);
        using var reader = new StreamReader(stream, Encoding.UTF8);
        var html = reader.ReadToEnd();

        var pageStart = html.IndexOf("id=\"ReelayConfigPage\"", StringComparison.Ordinal);
        var scriptStart = html.IndexOf("<script", StringComparison.Ordinal);
        var scriptEnd = html.IndexOf("</script>", StringComparison.Ordinal);
        var pageEnd = html.LastIndexOf("</div>", StringComparison.Ordinal);

        Assert.True(pageStart >= 0 && pageStart < scriptStart);
        Assert.True(scriptStart < scriptEnd && scriptEnd < pageEnd);
        Assert.Contains("data-require=\"emby-input,emby-button,emby-checkbox\"", html, StringComparison.Ordinal);
        Assert.Contains("autocomplete=\"new-password\"", html, StringComparison.Ordinal);
        Assert.DoesNotContain("${", html, StringComparison.Ordinal);
    }
}
