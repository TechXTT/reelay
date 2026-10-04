import { api } from "../api.ts";
import { hooks, session } from "../session.ts";
import type { DialogCheck, Grab } from "../types.ts";
import { confirmDialog, showError, showToast } from "../ui.ts";

export async function cancelGrab(grab: Grab, label: string) {
  const values = await confirmDialog({
    title: "Cancel download",
    message: `Stop ${label} and return it to the wanted queue?`,
    confirmLabel: "Cancel download",
    checks: [
      { id: "deleteData", label: "Delete downloaded data", detail: "Removes partial or completed source data from the download folder.", checked: true },
      { id: "blacklist", label: "Blacklist this release", detail: "Prevents Reelay from selecting the same release on its next search.", checked: true }
    ]
  });
  if (!values) return;
  try {
    await api(`/api/v1/queue/${grab.id}?deleteData=${values.deleteData}&blacklist=${values.blacklist}`,
      { method: "DELETE" });
    showToast("Download canceled");
    await hooks.navigate(session.current);
  } catch (error) {
    showError(error);
  }
}

export async function deleteCollection(kind: "movies" | "series", id: number, label: string,
  hasActive: boolean, hasFiles: boolean) {
  const checks: DialogCheck[] = [
    { id: "deleteDownloads", label: "Remove download-client data",
      detail: hasActive ? "Required because this collection has an active download." : "Removes Reelay torrents and their source data.",
      checked: hasActive, required: hasActive }
  ];
  if (hasFiles) checks.push({ id: "deleteFiles", label: "Delete imported library files",
    detail: "Removes only Reelay's imported files and matching subtitles inside the configured library root." });
  const values = await confirmDialog({
    title: `Delete ${kind === "movies" ? "movie" : "series"}`,
    message: `Remove ${label} from Reelay? Unselected files remain on disk.`,
    confirmLabel: "Delete",
    checks
  });
  if (!values) return;
  try {
    await api(`/api/v1/${kind}/${id}?deleteFiles=${Boolean(values.deleteFiles)}&deleteDownloads=${Boolean(values.deleteDownloads)}`,
      { method: "DELETE" });
    showToast(`${label} deleted`);
    await hooks.navigate(kind);
  } catch (error) {
    showError(error);
  }
}
