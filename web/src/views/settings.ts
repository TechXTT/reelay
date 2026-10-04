import { api, APIError, authToken, esc, setAuthToken } from "../api.ts";
import { session } from "../session.ts";
import type { ItemList, QualityProfile, SettingsResponse, SetupResponse } from "../types.ts";
import { bindAll, bindRequest, content, openDialog, pick, runControl, showToast, state, table } from "../ui.ts";

const triggerLoops = ["search", "status", "metadata", "recent", "recommendations"];

function connectionRows(settings: SettingsResponse) {
  return [
    ["HTTP server", `${esc(settings.server.bind)}:${esc(settings.server.port)}`, settings.server.auth_enabled ? "Token enabled" : "Loopback"],
    ["Download client", esc(settings.downloader.url), esc(settings.downloader.type)],
    ...(settings.indexers ?? []).map(indexer => [esc(indexer.name), esc(indexer.base_url), indexer.enabled ? "Enabled" : "Disabled"])
  ];
}

function profileRow(profile: QualityProfile) {
  return [esc(profile.name), esc(profile.allowed_resolutions.join(", ")), esc(profile.allowed_sources.join(", ")), esc(profile.min_seeders)];
}

function settingsMarkup(settings: SettingsResponse, profiles: QualityProfile[]) {
  return `<div class="page-head"><div><h1>Settings</h1><p>Runtime configuration</p></div><div class="row-actions"><button class="command" id="setup-checks">Setup checks</button><button class="command" id="database-backup">Download database backup</button></div></div>
    <section><h2>Access</h2><form id="token-form" class="inline-form"><input type="password" value="${esc(authToken())}" placeholder="Bearer token">
      <button class="command" type="submit">✓ <span>Save token</span></button></form></section>
    <section><h2>Connections</h2>${table(["Component", "Address", "Status"], connectionRows(settings))}</section>
    <section><h2>Library roots</h2>${table(["Type", "Path"], [["Series", esc(settings.library.TVRoot ?? settings.library.tv_root)], ["Movies", esc(settings.library.MovieRoot ?? settings.library.movie_root)]])}</section>
    <section><h2>Quality profiles</h2>${table(["Name", "Resolutions", "Sources", "Seeders"], profiles.map(profileRow))}</section>
    <section><h2>Recommendations</h2>${table(["Setting", "Value"], [["Enabled", settings.recommendations.enabled ? "Yes" : "No"], ["Refresh", esc(settings.recommendations.refresh_interval)], ["Results per user", esc(settings.recommendations.result_limit)]])}</section>
    <section><h2>Manual triggers</h2><div class="trigger-row">${triggerLoops.map(loop => `<button class="command trigger" data-loop="${loop}">↻ <span>${loop}</span></button>`).join("")}</div></section>`;
}

async function showSetupChecks() {
  const setup = await api<SetupResponse>("/api/v1/setup");
  openDialog(`<header class="preview-header"><h2>Setup checks</h2><button class="command setup-close" autofocus>Close</button></header><p>Availability webhook: ${setup.webhook_enabled ? "Enabled":"Not configured"}</p><ol class="setup-checks">${setup.checks.map(check=>`<li><strong>${esc(check.name)}</strong> ${state(check.status)}<p>${esc(check.detail || "")}${check.available_bytes!==undefined ? ` ${(check.available_bytes/1024**3).toFixed(1)} GiB available`:""}</p><p>${esc(check.action)}</p></li>`).join("")}</ol>`, ".setup-close").showModal();
}

async function downloadDatabaseBackup() {
  const headers = new Headers();
  if (authToken()) headers.set("Authorization", `Bearer ${authToken()}`);
  const response = await fetch("/api/v1/database/backup", { method: "POST", headers });
  if (!response.ok) throw new APIError(`Database backup failed (${response.status})`, response.status, "backup_failed");
  const download = document.createElement("a");
  const objectURL = URL.createObjectURL(await response.blob());
  download.href = objectURL;
  download.download = "reelay-backup.db";
  download.click();
  window.setTimeout(() => URL.revokeObjectURL(objectURL), 1000);
  showToast("Database backup downloaded");
}

export async function settingsView(onTokenSaved: () => void) {
  const [settings, profilePage] = await Promise.all([
    api<SettingsResponse>("/api/v1/settings"), api<ItemList<QualityProfile>>("/api/v1/profiles")
  ]);
  if (session.current !== "settings") return;
  const node = content(settingsMarkup(settings, profilePage.items ?? []));
  pick<HTMLFormElement>(node, "#token-form").onsubmit = event => {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    setAuthToken(pick<HTMLInputElement>(form, "input").value);
    onTokenSaved();
    showToast("Token saved");
  };
  bindAll<HTMLButtonElement>(node, ".trigger", button =>
    bindRequest(button, `/api/v1/system/trigger/${button.dataset.loop}`, { method: "POST" }, `${button.dataset.loop} triggered`));
  const setupChecks = pick<HTMLButtonElement>(node, "#setup-checks");
  setupChecks.onclick = () => void runControl(setupChecks, showSetupChecks);
  const backup = pick<HTMLButtonElement>(node, "#database-backup");
  backup.onclick = () => void runControl(backup, downloadDatabaseBackup);
}
