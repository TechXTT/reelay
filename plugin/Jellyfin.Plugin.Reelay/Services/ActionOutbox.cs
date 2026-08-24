using System.Text.Json;

namespace Jellyfin.Plugin.Reelay.Services;

public sealed record PendingAction(long RecommendationId, string ActionId, string Action);

public sealed class ActionOutbox
{
    private readonly object _gate = new();
    private readonly string _path;

    public ActionOutbox()
    {
        var folder = Plugin.Instance?.DataFolderPath ?? throw new InvalidOperationException("Plugin is not initialized");
        Directory.CreateDirectory(folder);
        _path = Path.Combine(folder, "action-outbox.json");
    }

    public void Enqueue(PendingAction action)
    {
        lock (_gate)
        {
            var values = Read();
            if (values.All(value => value.ActionId != action.ActionId)) values.Add(action);
            Write(values);
        }
    }

    public IReadOnlyList<PendingAction> Snapshot()
    {
        lock (_gate) return Read();
    }

    public void Complete(string actionId)
    {
        lock (_gate)
        {
            var values = Read();
            values.RemoveAll(value => value.ActionId == actionId);
            Write(values);
        }
    }

    private List<PendingAction> Read()
    {
        if (!File.Exists(_path)) return new List<PendingAction>();
        return JsonSerializer.Deserialize<List<PendingAction>>(File.ReadAllText(_path)) ?? new List<PendingAction>();
    }

    private void Write(List<PendingAction> values)
    {
        var temporary = _path + ".tmp";
        File.WriteAllText(temporary, JsonSerializer.Serialize(values));
        File.Move(temporary, _path, true);
    }
}
