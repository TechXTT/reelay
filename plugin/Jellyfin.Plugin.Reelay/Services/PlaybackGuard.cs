using MediaBrowser.Controller.Library;
using MediaBrowser.Controller.Session;
using MediaBrowser.Model.Entities;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed class PlaybackGuard : IHostedService
{
    private readonly ISessionManager _sessions;
    private readonly IUserManager _users;
    private readonly IUserDataManager _userData;
    private readonly VirtualLibraryManager _virtual;
    private readonly ILogger<PlaybackGuard> _logger;

    public PlaybackGuard(ISessionManager sessions, IUserManager users, IUserDataManager userData, VirtualLibraryManager virtualLibrary, ILogger<PlaybackGuard> logger)
    {
        _sessions = sessions;
        _users = users;
        _userData = userData;
        _virtual = virtualLibrary;
        _logger = logger;
    }

    public Task StartAsync(CancellationToken cancellationToken)
    {
        _sessions.PlaybackStart += OnPlayback;
        _sessions.PlaybackStopped += OnPlayback;
        return Task.CompletedTask;
    }

    public Task StopAsync(CancellationToken cancellationToken)
    {
        _sessions.PlaybackStart -= OnPlayback;
        _sessions.PlaybackStopped -= OnPlayback;
        return Task.CompletedTask;
    }

    private async void OnPlayback(object? sender, PlaybackProgressEventArgs e)
    {
        try
        {
            var item = e.Item;
            if (item is null || !_virtual.IsManagedPath(item.Path) || e.Session is null) return;
            await _sessions.SendPlaystateCommand(null, e.Session.Id, new MediaBrowser.Model.Session.PlaystateRequest { Command = MediaBrowser.Model.Session.PlaystateCommand.Stop, ControllingUserId = e.Session.UserId.ToString() }, CancellationToken.None).ConfigureAwait(false);
            var user = _users.GetUserById(e.Session.UserId);
            if (user is null) return;
            var data = _userData.GetUserData(user, item);
            if (data is null) return;
            data.Played = false;
            data.PlaybackPositionTicks = 0;
            data.LastPlayedDate = null;
            _userData.SaveUserData(user, item, data, UserDataSaveReason.UpdateUserData, CancellationToken.None);
        }
        catch (Exception ex) { _logger.LogWarning(ex, "Could not stop or clear virtual Reelay playback"); }
    }
}
