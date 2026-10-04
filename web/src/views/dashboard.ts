import { api, esc } from "../api.ts";
import { session } from "../session.ts";
import type { Grab, HealthComponent, HealthResponse, HistoryResponse, ItemList, Movie, QueueResponse } from "../types.ts";
import { bindAll, bindRequest, content, date, isEditing, percent, pick, setConnection, showError, showToast, state, table } from "../ui.ts";
import { cancelGrab, deleteCollection } from "./collection.ts";

function downloadLabel(grab: Grab, movieNames: Map<number, string>) {
  if (grab.subject_type === "movie" && movieNames.has(grab.subject_id)) return movieNames.get(grab.subject_id)!;
  const parts = String(grab.content_path || "").replaceAll("\\", "/").split("/").filter(Boolean);
  return parts.at(-1) || `${grab.subject_type} #${grab.subject_id}`;
}

function dashboardHead(activeCount: number, downloadsPaused: boolean) {
  return `<div class="page-head"><div><h1>Dashboard</h1><p>${activeCount} active downloads${downloadsPaused ? " · paused" : ""}</p></div>
    <div class="row-actions"><button class="command" id="pause-downloads" title="Pause all downloads" ${downloadsPaused ? "disabled" : ""}>Ⅱ <span>Pause all</span></button>
    <button class="command" id="resume-downloads" title="Resume all downloads" ${downloadsPaused ? "" : "disabled"}>▶ <span>Resume all</span></button>
    <button class="command" id="refresh-search">↻ <span>Run search</span></button></div></div>`;
}

function downloadCard(grab: Grab, movieNames: Map<number, string>, movieByID: Map<number, Movie>) {
  return `
      <article class="download"><div class="download-title"><strong title="${esc(grab.content_path)}">${esc(downloadLabel(grab, movieNames))}</strong>
      <small>${esc(grab.subject_type)} #${esc(grab.subject_id)}</small></div>${state(grab.state)}
      <div class="progress"><i style="width:${percent(grab.progress)}%"></i></div>
      <small class="progress-label">${(grab.progress * 100).toFixed(1)}%</small>
      <div class="row-actions"><button class="command compact danger cancel-grab" data-grab="${grab.id}">Cancel download</button>
      ${grab.subject_type === "movie" && movieByID.has(grab.subject_id) ? `<button class="command compact danger delete-active-movie" data-movie="${grab.subject_id}">Delete movie</button>` : ""}</div></article>`;
}

function healthRow(component: HealthComponent) {
  return `<div class="health-row">
      <span class="health-dot ${esc(component.status)}"></span><div><strong>${esc(component.name)}</strong><small>${esc(component.detail || component.kind)}</small></div>${state(component.status)}</div>`;
}

function recentActivityRow(grab: Grab) {
  return [`${grab.subject_type} #${grab.subject_id}`, state(grab.state), `${(grab.progress * 100).toFixed(0)}%`, date(grab.updated_at)];
}

async function setDownloadsPaused(paused: boolean) {
  const button = pick<HTMLButtonElement>(document, paused ? "#pause-downloads" : "#resume-downloads");
  button.disabled = true;
  try {
    await api(`/api/v1/queue/${paused ? "pause" : "resume"}`, { method: "POST" });
    showToast(paused ? "All downloads paused" : "All downloads resumed");
    await dashboard();
  } catch (error) {
    showError(error);
    button.disabled = false;
  }
}

export async function dashboard() {
  const [health, queue, history, movies] = await Promise.all([
    api<HealthResponse>("/api/v1/health", {}, [503]), api<QueueResponse>("/api/v1/queue"),
    api<HistoryResponse>("/api/v1/history?page=1"), api<ItemList<Movie>>("/api/v1/movies")
  ]);
  if (session.current !== "dashboard" || (session.refreshing && isEditing())) return;
  const active = queue.items ?? [];
  const downloadsPaused = Boolean(queue.paused);
  const components = health.components ?? [];
  const movieByID = new Map((movies.items ?? []).map(movie => [movie.id, movie]));
  const movieNames = new Map(Array.from(movieByID, ([id, movie]) => [id, movie.title]));
  const recent = (history.items ?? []).filter(grab => grab.state !== "removed" &&
    (grab.subject_type !== "movie" || movieByID.has(grab.subject_id))).slice(0, 8);
  content(`${dashboardHead(active.length, downloadsPaused)}
    <section><h2>Active downloads</h2><div class="downloads">${active.length ? active.map(grab => downloadCard(grab, movieNames, movieByID)).join("") : `<div class="empty">No active downloads</div>`}</div></section>
    <section><h2>System health</h2><div class="health-grid">${components.map(healthRow).join("")}</div></section>
    <section><h2>Recent activity</h2>${table(["Subject", "State", "Progress", "Updated"], recent.map(recentActivityRow))}</section>`);
  bindRequest(pick<HTMLButtonElement>(document, "#refresh-search"), "/api/v1/system/trigger/search", { method: "POST" }, "Search triggered");
  pick<HTMLButtonElement>(document, "#pause-downloads").onclick = () => void setDownloadsPaused(true);
  pick<HTMLButtonElement>(document, "#resume-downloads").onclick = () => void setDownloadsPaused(false);
  bindAll<HTMLButtonElement>(document, ".cancel-grab", button => button.onclick = () => {
    const grab = active.find(item => item.id === Number(button.dataset.grab));
    if (grab) void cancelGrab(grab, downloadLabel(grab, movieNames));
  });
  bindAll<HTMLButtonElement>(document, ".delete-active-movie", button => button.onclick = () => {
    const movie = movieByID.get(Number(button.dataset.movie));
    if (movie) void deleteCollection("movies", movie.id, movie.title, true, Boolean(movie.imported_path));
  });
  setConnection(health.status);
}
