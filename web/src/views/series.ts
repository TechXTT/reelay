import { api, esc } from "../api.ts";
import { hooks, session } from "../session.ts";
import type { Episode, ItemList, QualityProfile, QueueResponse, Series, SeriesDetailResponse, SeriesPatch } from "../types.ts";
import { bindAll, bindRequest, content, date, isEditing, option, pad, pick, profileOptions, runControl, showError, showToast, state, table } from "../ui.ts";
import { deleteCollection } from "./collection.ts";

function seriesCard(item: Series) {
  return `<button class="media-card" data-series="${item.id}">
      <span class="media-monogram">${esc(item.title.charAt(0))}</span><div><strong>${esc(item.title)}</strong>
      <small>${esc(item.year || "Year unknown")} · ${esc(item.monitor_mode)}</small></div>${state(item.status)}</button>`;
}

export async function seriesView() {
  const payload = await api<ItemList<Series>>("/api/v1/series");
  if (session.current !== "series" || (session.refreshing && isEditing())) return;
  const items = payload.items ?? [];
  const node = content(`<div class="page-head"><div><h1>Series</h1><p>${items.length} followed</p></div>
    <button class="command" data-view="add">+ <span>Add series</span></button></div>
    <div class="media-grid">${items.length ? items.map(seriesCard).join("") : `<div class="empty">No followed series</div>`}</div>
    <section id="series-detail"></section>`);
  pick<HTMLElement>(node, "[data-view=add]").onclick = () => hooks.navigate("add");
  bindAll<HTMLElement>(node, "[data-series]", element => element.onclick = () =>
    void seriesDetail(Number(element.dataset.series)).catch(showError));
}

function episodeRow(episode: Episode) {
  return [
    `S${pad(episode.season)}E${pad(episode.number)}`, esc(episode.title || "Untitled"), date(episode.air_date), state(episode.state),
    `<button class="command compact episode-search" data-id="${episode.id}">Search</button>`
  ];
}

function seriesDetailMarkup(item: Series, episodes: Episode[], profiles: QualityProfile[]) {
  return `<div class="section-head"><div><h2>${esc(item.title)}</h2><p>${esc(item.monitor_mode)} monitoring</p></div>
    <div class="row-actions"><button class="command" id="series-search">Search</button>
    <button class="command danger" id="series-delete">Delete</button></div></div>
    <div class="manage-bar"><label>Profile<select id="series-profile">${profileOptions(profiles, item.quality_profile_id)}</select></label>
    <label>Monitoring<select id="series-monitor">
      ${option("future_only", "Future only", item.monitor_mode)}${option("all", "All episodes", item.monitor_mode)}
      ${option("latest_season", "Latest season", item.monitor_mode)}${option("none", "None", item.monitor_mode)}</select></label>
    <label>Status<select id="series-status">${option("following", "Following", item.status)}
      ${option("paused", "Paused", item.status)}${option("ended", "Ended", item.status)}</select></label></div>
    ${table(["Episode", "Title", "Air date", "State", ""], episodes.map(episodeRow))}`;
}

async function patchSeries(item: Series, body: SeriesPatch) {
  await api(`/api/v1/series/${item.id}`, { method: "PATCH", body: JSON.stringify(body) });
  if (body.profile_id !== undefined) item.quality_profile_id = body.profile_id;
  if (body.monitor_mode !== undefined) item.monitor_mode = body.monitor_mode;
  if (body.status !== undefined) item.status = body.status;
  showToast("Series updated");
}

export async function seriesDetail(id: number) {
  session.selectedSeries = id;
  const [payload, profilePage, queue] = await Promise.all([
    api<SeriesDetailResponse>(`/api/v1/series/${id}`), api<ItemList<QualityProfile>>("/api/v1/profiles"),
    api<QueueResponse>("/api/v1/queue")
  ]);
  if (session.current !== "series" || session.selectedSeries !== id || (session.refreshing && isEditing())) return;
  const item = payload.series;
  const episodes = payload.episodes ?? [];
  const episodeIDs = new Set(episodes.map(episode => episode.id));
  const hasActive = (queue.items ?? []).some(grab => grab.subject_type === "episode" && episodeIDs.has(grab.subject_id));
  const hasFiles = episodes.some(episode => Boolean(episode.imported_path));
  const detail = pick<HTMLElement>(document, "#series-detail");
  detail.innerHTML = seriesDetailMarkup(item, episodes, profilePage.items ?? []);
  bindRequest(pick<HTMLButtonElement>(detail, "#series-search"), `/api/v1/series/${id}/search`, { method: "POST" }, "Series search started");
  bindAll<HTMLButtonElement>(detail, ".episode-search", button =>
    bindRequest(button, `/api/v1/episodes/${button.dataset.id}/search`, { method: "POST" }, "Episode search started"));
  bindSeriesField(detail, item, "#series-profile", "quality_profile_id", value => ({ profile_id: Number(value) }));
  bindSeriesField(detail, item, "#series-monitor", "monitor_mode", value => ({ monitor_mode: value as Series["monitor_mode"] }));
  bindSeriesField(detail, item, "#series-status", "status", value => ({ status: value as Series["status"] }));
  pick<HTMLButtonElement>(detail, "#series-delete").onclick = () =>
    void deleteCollection("series", id, item.title, hasActive, hasFiles);
}

function bindSeriesField(detail: HTMLElement, item: Series, selector: string,
  stored: "quality_profile_id" | "monitor_mode" | "status", toPatch: (value: string) => SeriesPatch) {
  const select = pick<HTMLSelectElement>(detail, selector);
  select.onchange = () => void runControl(select, () => patchSeries(item, toPatch(select.value)),
    () => { select.value = String(item[stored]); });
}
