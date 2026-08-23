import "./styles.css";
import { api, APIError, authToken, connectEvents, esc, setAuthToken } from "./api.ts";

type View = "dashboard" | "series" | "movies" | "add" | "settings";
type Item = Record<string, any>;

const app = document.querySelector<HTMLDivElement>("#app")!;
let current: View = "dashboard";
let eventSource: EventSource | null = null;
let refreshTimer = 0;

const nav: { id: View; label: string; icon: string }[] = [
  { id: "dashboard", label: "Dashboard", icon: "◫" },
  { id: "series", label: "Series", icon: "▤" },
  { id: "movies", label: "Movies", icon: "▶" },
  { id: "add", label: "Add", icon: "+" },
  { id: "settings", label: "Settings", icon: "⚙" }
];

function shell(): void {
  app.innerHTML = `<header class="topbar"><button class="brand" data-view="dashboard" aria-label="Dashboard">
    <span class="brand-mark">R</span><strong>Reelay</strong></button>
    <div id="connection" class="connection">Connecting</div></header>
    <div class="layout"><nav>${nav.map(n => `<button data-view="${n.id}" title="${n.label}" class="${current === n.id ? "active" : ""}">
      <span aria-hidden="true">${n.icon}</span><span>${n.label}</span></button>`).join("")}</nav>
    <main id="content"><div class="loading">Loading</div></main></div>
    <div id="toast" role="status" aria-live="polite"></div>`;
  document.querySelectorAll<HTMLElement>("[data-view]").forEach(el => el.onclick = () => navigate(el.dataset.view as View));
}

async function navigate(view: View): Promise<void> {
  current = view;
  shell();
  try {
    if (view === "dashboard") await dashboard();
    if (view === "series") await seriesView();
    if (view === "movies") await moviesView();
    if (view === "add") await addView();
    if (view === "settings") await settingsView();
  } catch (error) {
    if (error instanceof APIError && error.status === 401) {
      authGate(error.message);
      return;
    }
    showError(error);
  }
}

function content(html: string): HTMLElement {
  const node = document.querySelector<HTMLElement>("#content")!;
  node.innerHTML = html;
  return node;
}

function state(value: string): string {
  return `<span class="state state-${esc(value)}">${esc(value.replaceAll("_", " "))}</span>`;
}

function showError(error: unknown): void {
  const toast = document.querySelector<HTMLElement>("#toast");
  if (!toast) return;
  toast.textContent = error instanceof Error ? error.message : String(error);
  toast.className = "show error";
  window.setTimeout(() => toast.className = "", 4500);
}

function authGate(message: string): void {
  eventSource?.close();
  eventSource = null;
  setConnection("Authentication required");
  const node = content(`<div class="auth-gate"><div><span class="brand-mark">R</span>
    <h1>Authentication required</h1><p>${esc(message)}</p></div>
    <form id="auth-form"><label for="auth-token">Bearer token</label>
      <input id="auth-token" name="token" type="password" autocomplete="current-password"
        autocapitalize="none" spellcheck="false" required autofocus>
      <button class="command" type="submit">Connect</button></form></div>`);
  node.querySelector<HTMLFormElement>("#auth-form")!.onsubmit = event => {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    setAuthToken(new FormData(form).get("token") as string);
    connect();
    void navigate(current);
  };
}

async function dashboard(): Promise<void> {
  const [health, queue, history] = await Promise.all([
    api<Item>("/api/v1/health"), api<Item>("/api/v1/queue"), api<Item>("/api/v1/history?page=1")
  ]);
  const active = queue.items ?? [];
  const recent = (history.items ?? []).slice(0, 8);
  const components = health.components ?? [];
  content(`<div class="page-head"><div><h1>Dashboard</h1><p>${active.length} active downloads</p></div>
    <button class="command" id="refresh-search">↻ <span>Run search</span></button></div>
    <section><h2>Active downloads</h2><div class="downloads">${active.length ? active.map((g: Item) => `
      <article class="download"><div><strong>${esc(g.torrent_hash.slice(0, 10))}</strong>${state(g.state)}</div>
      <div class="progress"><i style="width:${Math.max(0, Math.min(100, g.progress * 100))}%"></i></div>
      <small>${(g.progress * 100).toFixed(1)}%</small></article>`).join("") : `<div class="empty">No active downloads</div>`}</div></section>
    <section><h2>System health</h2><div class="health-grid">${components.map((c: Item) => `<div class="health-row">
      <span class="health-dot ${esc(c.status)}"></span><div><strong>${esc(c.name)}</strong><small>${esc(c.detail || c.kind)}</small></div>${state(c.status)}</div>`).join("")}</div></section>
    <section><h2>Recent activity</h2>${table(["Subject", "State", "Progress", "Updated"], recent.map((g: Item) => [
      `${g.subject_type} #${g.subject_id}`, state(g.state), `${(g.progress * 100).toFixed(0)}%`, date(g.updated_at)
    ]))}</section>`);
  document.querySelector<HTMLButtonElement>("#refresh-search")!.onclick = async () => {
    await api("/api/v1/system/trigger/search", { method: "POST" }); showToast("Search triggered");
  };
  setConnection(health.status);
}

async function seriesView(): Promise<void> {
  const payload = await api<Item>("/api/v1/series");
  const items = payload.items ?? [];
  const node = content(`<div class="page-head"><div><h1>Series</h1><p>${items.length} followed</p></div>
    <button class="command" data-view="add">+ <span>Add series</span></button></div>
    <div class="media-grid">${items.length ? items.map((item: Item) => `<button class="media-card" data-series="${item.id}">
      <span class="media-monogram">${esc(item.title.charAt(0))}</span><div><strong>${esc(item.title)}</strong>
      <small>${esc(item.year || "Year unknown")} · ${esc(item.monitor_mode)}</small></div>${state(item.status)}</button>`).join("") : `<div class="empty">No followed series</div>`}</div>
    <section id="series-detail"></section>`);
  node.querySelector<HTMLElement>("[data-view=add]")!.onclick = () => navigate("add");
  node.querySelectorAll<HTMLElement>("[data-series]").forEach(el => el.onclick = () => seriesDetail(Number(el.dataset.series)));
}

async function seriesDetail(id: number): Promise<void> {
  const payload = await api<Item>(`/api/v1/series/${id}`);
  const item = payload.series, episodes = payload.episodes ?? [];
  const detail = document.querySelector<HTMLElement>("#series-detail")!;
  detail.innerHTML = `<div class="section-head"><div><h2>${esc(item.title)}</h2><p>${esc(item.monitor_mode)} monitoring</p></div>
    <button class="icon-button" id="series-search" title="Search now">↻</button></div>
    ${table(["Episode", "Title", "Air date", "State", ""], episodes.map((e: Item) => [
      `S${pad(e.season)}E${pad(e.number)}`, esc(e.title || "Untitled"), date(e.air_date), state(e.state),
      `<button class="icon-button episode-search" data-id="${e.id}" title="Search now">↻</button>`
    ]))}`;
  detail.querySelector<HTMLButtonElement>("#series-search")!.onclick = async () => {
    await api(`/api/v1/series/${id}/search`, { method: "POST" }); showToast("Series search started");
  };
  detail.querySelectorAll<HTMLButtonElement>(".episode-search").forEach(button => button.onclick = async () => {
    await api(`/api/v1/episodes/${button.dataset.id}/search`, { method: "POST" }); showToast("Episode search started");
  });
}

async function moviesView(): Promise<void> {
  const payload = await api<Item>("/api/v1/movies");
  const items = payload.items ?? [];
  const node = content(`<div class="page-head"><div><h1>Movies</h1><p>${items.length} tracked</p></div>
    <button class="command" data-view="add">+ <span>Add movie</span></button></div>
    ${table(["Title", "Year", "State", "Attempts", ""], items.map((m: Item) => [esc(m.title), esc(m.year), state(m.state), esc(m.search_attempts),
      `<button class="icon-button movie-search" data-id="${m.id}" title="Search now">↻</button>`]))}`);
  node.querySelector<HTMLElement>("[data-view=add]")!.onclick = () => navigate("add");
  node.querySelectorAll<HTMLButtonElement>(".movie-search").forEach(button => button.onclick = async () => {
    await api(`/api/v1/movies/${button.dataset.id}/search`, { method: "POST" }); showToast("Movie search started");
  });
}

async function addView(): Promise<void> {
  const profiles = (await api<Item>("/api/v1/profiles")).items ?? [];
  const node = content(`<div class="page-head"><div><h1>Add</h1><p>Find a movie or series</p></div></div>
    <form id="add-search" class="search-form"><div class="segmented"><label><input type="radio" name="type" value="series" checked><span>Series</span></label>
      <label><input type="radio" name="type" value="movie"><span>Movie</span></label></div>
      <input name="query" type="search" required placeholder="Title" autocomplete="off"><button class="command" type="submit">⌕ <span>Search</span></button></form>
    <div id="search-results" class="search-results"></div>`);
  node.querySelector<HTMLFormElement>("#add-search")!.onsubmit = async event => {
    event.preventDefault();
	const form = event.currentTarget as HTMLFormElement;
	const data = new FormData(form);
    const type = String(data.get("type")), query = String(data.get("query"));
    const payload = await api<Item>(`/api/v1/metadata/search?type=${type}&q=${encodeURIComponent(query)}`);
    const results = document.querySelector<HTMLElement>("#search-results")!;
    results.innerHTML = (payload.items ?? []).map((item: Item, index: number) => `<article class="result">
      ${item.poster_url ? `<img src="${esc(item.poster_url)}" alt="">` : `<div class="poster-fallback">${esc(item.title.charAt(0))}</div>`}
      <div><h2>${esc(item.title)}</h2><p>${esc(item.year || "Year unknown")}</p><small>${esc(item.overview || "")}</small></div>
      <div class="add-controls"><select data-profile>${profiles.map((p: Item) => `<option value="${p.id}" ${p.is_default ? "selected" : ""}>${esc(p.name)}</option>`).join("")}</select>
      ${type === "series" ? `<select data-monitor><option value="future_only">Future only</option><option value="all">All episodes</option><option value="latest_season">Latest season</option></select>` : ""}
      <button class="command add-result" data-index="${index}">+ <span>Add</span></button></div></article>`).join("") || `<div class="empty">No matches</div>`;
    results.querySelectorAll<HTMLButtonElement>(".add-result").forEach(button => button.onclick = async () => {
      const result = payload.items[Number(button.dataset.index)];
      const parent = button.closest<HTMLElement>(".result")!;
      const profile_id = Number(parent.querySelector<HTMLSelectElement>("[data-profile]")!.value);
      const body = type === "series" ? { query: result.title, tvmaze_id: result.tvmaze_id, profile_id,
        monitor_mode: parent.querySelector<HTMLSelectElement>("[data-monitor]")!.value } :
        { query: result.title, tmdb_id: result.tmdb_id, year: result.year, profile_id };
      await api(`/api/v1/${type === "series" ? "series" : "movies"}`, { method: "POST", body: JSON.stringify(body) });
      button.disabled = true; button.textContent = "Added"; showToast(`${result.title} added`);
    });
  };
}

async function settingsView(): Promise<void> {
  const [settings, profiles] = await Promise.all([api<Item>("/api/v1/settings"), api<Item>("/api/v1/profiles")]);
  const node = content(`<div class="page-head"><div><h1>Settings</h1><p>Runtime configuration</p></div></div>
    <section><h2>Access</h2><form id="token-form" class="inline-form"><input type="password" value="${esc(authToken())}" placeholder="Bearer token">
      <button class="command" type="submit">✓ <span>Save token</span></button></form></section>
    <section><h2>Connections</h2>${table(["Component", "Address", "Status"], [
      ["HTTP server", `${esc(settings.server.bind)}:${esc(settings.server.port)}`, settings.server.auth_enabled ? "Token enabled" : "Loopback"],
      ["Download client", esc(settings.downloader.url), esc(settings.downloader.type)],
      ...(settings.indexers ?? []).map((i: Item) => [esc(i.name), esc(i.base_url), i.enabled ? "Enabled" : "Disabled"])
    ])}</section>
    <section><h2>Library roots</h2>${table(["Type", "Path"], [["Series", esc(settings.library.TVRoot ?? settings.library.tv_root)], ["Movies", esc(settings.library.MovieRoot ?? settings.library.movie_root)]])}</section>
    <section><h2>Quality profiles</h2>${table(["Name", "Resolutions", "Sources", "Seeders"], (profiles.items ?? []).map((p: Item) => [
      esc(p.name), esc(p.allowed_resolutions.join(", ")), esc(p.allowed_sources.join(", ")), esc(p.min_seeders)
    ]))}</section>
    <section><h2>Manual triggers</h2><div class="trigger-row">${["search", "status", "metadata", "recent"].map(loop => `<button class="command trigger" data-loop="${loop}">↻ <span>${loop}</span></button>`).join("")}</div></section>`);
  node.querySelector<HTMLFormElement>("#token-form")!.onsubmit = event => {
	event.preventDefault();
	const form = event.currentTarget as HTMLFormElement;
	setAuthToken(form.querySelector("input")!.value); connect(); showToast("Token saved");
  };
  node.querySelectorAll<HTMLButtonElement>(".trigger").forEach(button => button.onclick = async () => {
    await api(`/api/v1/system/trigger/${button.dataset.loop}`, { method: "POST" }); showToast(`${button.dataset.loop} triggered`);
  });
}

function table(headers: string[], rows: string[][]): string {
  return `<div class="table-wrap"><table><thead><tr>${headers.map(h => `<th>${h}</th>`).join("")}</tr></thead><tbody>
    ${rows.length ? rows.map(row => `<tr>${row.map(cell => `<td>${cell}</td>`).join("")}</tr>`).join("") : `<tr><td colspan="${headers.length}" class="empty">No records</td></tr>`}
    </tbody></table></div>`;
}
const pad = (n: number) => String(n).padStart(2, "0");
const date = (value: string) => value ? new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: value.includes("T") ? "short" : undefined }).format(new Date(value)) : "—";
function showToast(message: string): void { const toast = document.querySelector<HTMLElement>("#toast")!; toast.textContent = message; toast.className = "show"; setTimeout(() => toast.className = "", 3000); }
function setConnection(status: string): void { const el = document.querySelector<HTMLElement>("#connection"); if (el) { el.textContent = status === "ok" ? "Connected" : status; el.className = `connection ${status}`; } }
function connect(): void { eventSource?.close(); eventSource = connectEvents(() => { clearTimeout(refreshTimer); refreshTimer = window.setTimeout(() => navigate(current), 350); }); eventSource.onopen = () => setConnection("ok"); eventSource.onerror = () => setConnection("offline"); }

shell(); connect(); navigate("dashboard");
