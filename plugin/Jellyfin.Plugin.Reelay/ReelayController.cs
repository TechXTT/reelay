using Jellyfin.Plugin.Reelay.Services;
using Microsoft.AspNetCore.Authorization;
using Microsoft.AspNetCore.Mvc;

namespace Jellyfin.Plugin.Reelay;

[ApiController]
[Route("Reelay")]
[Authorize(Policy = "RequiresElevation")]
public sealed class ReelayController : ControllerBase
{
    private readonly ReelayClient _client;

    public ReelayController(ReelayClient client)
    {
        _client = client;
    }

    [HttpPost("Test")]
    public async Task<IActionResult> TestConnection([FromBody] ConnectionSettings settings, CancellationToken cancellationToken)
    {
        try
        {
            await _client.TestAsync(settings.Url, settings.Token, cancellationToken).ConfigureAwait(false);
            return Ok(new { ok = true });
        }
        catch (Exception error) when (error is ArgumentException or HttpRequestException or TaskCanceledException)
        {
            return BadRequest(new { error = error.Message });
        }
    }
}

public sealed record ConnectionSettings(string Url, string Token);
