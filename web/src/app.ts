import "./styles.css";
import { api, APIError, authToken, connectEvents, esc, setAuthToken } from "./api.ts";

type View = "dashboard" | "discover" | "series" | "movies" | "add" | "settings";
type Item = Record<string, any>;

const app = document.querySelector<HTMLDivElement>("#app")!;
let current: View = "dashboard";
let eventSource: EventSource | null = null;
let refreshTimer = 0;
const discoverUserKey = "reelay.discover-user";
const discoverTypeKey = "reelay.discover-type";
let discoverUser = localStorage.getItem(discoverUserKey) ?? "";
let discoverType = localStorage.getItem(discoverTypeKey) === "series" ? "series" : "movie";

type DialogCheck = { id: string; label: string; detail: string; checked?: boolean; required?: boolean };

const nav: { id: View; label: string; icon: string }[] = [
  { id: "dashboard", label: "Dashboard", icon: "◫" },
  { id: "discover", label: "Discover", icon: "*" },
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
    if (view === "discover") await discoverView();
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
  const [health, queue, history, movies] = await Promise.all([
    api<Item>("/api/v1/health"), api<Item>("/api/v1/queue"), api<Item>("/api/v1/history?page=1"),
    api<Item>("/api/v1/movies")
  ]);
  const active = queue.items ?? [];
  const downloadsPaused = Boolean(queue.paused);
  const components = health.components ?? [];
  const movieByID = new Map<number, Item>((movies.items ?? []).map((movie: Item) => [movie.id, movie]));
  const movieNames = new Map<number, string>(Array.from(movieByID, ([id, movie]) => [id, movie.title]));
  const recent = (history.items ?? []).filter((grab: Item) => grab.state !== "removed" &&
    (grab.subject_type !== "movie" || movieByID.has(grab.subject_id))).slice(0, 8);
  content(`<div class="page-head"><div><h1>Dashboard</h1><p>${active.length} active downloads${downloadsPaused ? " · paused" : ""}</p></div>
    <div class="row-actions"><button class="command" id="pause-downloads" title="Pause all downloads" ${downloadsPaused ? "disabled" : ""}>Ⅱ <span>Pause all</span></button>
    <button class="command" id="resume-downloads" title="Resume all downloads" ${downloadsPaused ? "" : "disabled"}>▶ <span>Resume all</span></button>
    <button class="command" id="refresh-search">↻ <span>Run search</span></button></div></div>
    <section><h2>Active downloads</h2><div class="downloads">${active.length ? active.map((g: Item) => `
      <article class="download"><div class="download-title"><strong title="${esc(g.content_path)}">${esc(downloadLabel(g, movieNames))}</strong>
      <small>${esc(g.subject_type)} #${esc(g.subject_id)}</small></div>${state(g.state)}
      <div class="progress"><i style="width:${Math.max(0, Math.min(100, g.progress * 100))}%"></i></div>
      <small class="progress-label">${(g.progress * 100).toFixed(1)}%</small>
      <div class="row-actions"><button class="command compact danger cancel-grab" data-grab="${g.id}">Cancel download</button>
      ${g.subject_type === "movie" && movieByID.has(g.subject_id) ? `<button class="command compact danger delete-active-movie" data-movie="${g.subject_id}">Delete movie</button>` : ""}</div></article>`).join("") : `<div class="empty">No active downloads</div>`}</div></section>
    <section><h2>System health</h2><div class="health-grid">${components.map((c: Item) => `<div class="health-row">
      <span class="health-dot ${esc(c.status)}"></span><div><strong>${esc(c.name)}</strong><small>${esc(c.detail || c.kind)}</small></div>${state(c.status)}</div>`).join("")}</div></section>
    <section><h2>Recent activity</h2>${table(["Subject", "State", "Progress", "Updated"], recent.map((g: Item) => [
      `${g.subject_type} #${g.subject_id}`, state(g.state), `${(g.progress * 100).toFixed(0)}%`, date(g.updated_at)
    ]))}</section>`);
  document.querySelector<HTMLButtonElement>("#refresh-search")!.onclick = async () => {
    await api("/api/v1/system/trigger/search", { method: "POST" }); showToast("Search triggered");
  };
  const setPaused = async (paused: boolean): Promise<void> => {
    const button = document.querySelector<HTMLButtonElement>(paused ? "#pause-downloads" : "#resume-downloads")!;
    button.disabled = true;
    try {
      await api(`/api/v1/queue/${paused ? "pause" : "resume"}`, { method: "POST" });
      showToast(paused ? "All downloads paused" : "All downloads resumed");
      await dashboard();
    } catch (error) {
      showError(error);
      button.disabled = false;
    }
  };
  document.querySelector<HTMLButtonElement>("#pause-downloads")!.onclick = () => void setPaused(true);
  document.querySelector<HTMLButtonElement>("#resume-downloads")!.onclick = () => void setPaused(false);
  document.querySelectorAll<HTMLButtonElement>(".cancel-grab").forEach(button => button.onclick = () => {
    const grab = active.find((item: Item) => item.id === Number(button.dataset.grab));
    if (grab) void cancelGrab(grab, downloadLabel(grab, movieNames));
  });
  document.querySelectorAll<HTMLButtonElement>(".delete-active-movie").forEach(button => button.onclick = () => {
    const movie = movieByID.get(Number(button.dataset.movie));
    if (movie) void deleteCollection("movies", movie.id, movie.title, true, Boolean(movie.imported_path));
  });
  setConnection(health.status);
}

async function discoverView(selectedUser = discoverUser, mediaType = discoverType): Promise<void> {
  const users = (await api<Item>("/api/v1/integrations/jellyfin/users")).items ?? [];
  if (!users.length) {
    content(`<div class="page-head"><div><h1>Discover</h1><p>Personalized from Jellyfin activity</p></div></div>
      <div class="empty">Install and configure the Reelay Jellyfin plugin to synchronize users.</div>`);
    return;
  }
  const user = users.find((value: Item) => `${value.server_id}:${value.user_id}` === selectedUser) ?? users[0];
  const key = `${user.server_id}:${user.user_id}`;
  discoverUser = key;
  discoverType = mediaType;
  localStorage.setItem(discoverUserKey, key);
  localStorage.setItem(discoverTypeKey, mediaType);
  const query = `server_id=${encodeURIComponent(user.server_id)}&user_id=${encodeURIComponent(user.user_id)}&media_type=${mediaType}`;
  const values = (await api<Item>(`/api/v1/recommendations?${query}`)).items ?? [];
  const node = content(`<div class="page-head"><div><h1>Discover</h1><p>Recommendations for ${esc(user.display_name)}</p></div>
    <button class="command" id="generate-recommendations">Refresh</button></div>
    <div class="discover-toolbar"><label>User<select id="discover-user">${users.map((value: Item) => option(`${value.server_id}:${value.user_id}`, value.display_name, key)).join("")}</select></label>
    <div class="segmented"><label><input type="radio" name="discover-type" value="movie" ${mediaType === "movie" ? "checked" : ""}><span>Movies</span></label>
    <label><input type="radio" name="discover-type" value="series" ${mediaType === "series" ? "checked" : ""}><span>Series</span></label></div></div>
    <div class="recommendation-grid">${values.length ? values.map((item: Item) => `<article class="recommendation-card">
      ${item.poster_url ? `<img src="${esc(item.poster_url)}" alt="">` : `<div class="recommendation-poster">${esc(item.title.charAt(0))}</div>`}
      <div class="recommendation-body"><div class="recommendation-title"><div><h2>${esc(item.title)}</h2><small>${esc(item.year || "Year unknown")}</small></div><strong>${Number(item.score).toFixed(0)}</strong></div>
      <p>${esc(item.overview || "")}</p><ul>${(item.reasons ?? []).map((reason: string) => `<li>${esc(reason)}</li>`).join("")}</ul>
      <div class="row-actions recommendation-actions"><button class="command compact rec-dismiss" data-id="${item.id}">Dismiss</button>
      <label class="rating-field"><span>Rating</span><select class="rec-rating" aria-label="Rating for ${esc(item.title)}"><option value="1">1</option><option value="2">2</option><option value="3">3</option><option value="4">4</option><option value="5" selected>5</option></select></label>
      <button class="command compact rec-rate" data-id="${item.id}">Rate</button><button class="command compact rec-request" data-id="${item.id}">Request</button></div></div>
    </article>`).join("") : `<div class="empty">No active recommendations</div>`}</div>`);
  node.querySelector<HTMLSelectElement>("#discover-user")!.onchange = event => void discoverView((event.currentTarget as HTMLSelectElement).value, mediaType);
  node.querySelectorAll<HTMLInputElement>("[name=discover-type]").forEach(input => input.onchange = () => void discoverView(key, input.value));
  node.querySelector<HTMLButtonElement>("#generate-recommendations")!.onclick = async () => {
    await api("/api/v1/recommendations/generate", { method: "POST", body: JSON.stringify({ server_id: user.server_id, user_id: user.user_id, media_type: mediaType }) });
    showToast("Recommendations refreshed"); await discoverView(key, mediaType);
  };
  for (const action of ["dismiss", "request"] as const) {
    node.querySelectorAll<HTMLButtonElement>(`.rec-${action}`).forEach(button => {
      button.onclick = () => void recommendationAction(button, action);
    });
  }
  node.querySelectorAll<HTMLButtonElement>(".rec-rate").forEach(button => {
    button.onclick = () => void recommendationAction(button, "rate");
  });
}

async function recommendationAction(button: HTMLButtonElement, action: "dismiss" | "request" | "rate"): Promise<void> {
  const card = button.closest<HTMLElement>(".recommendation-card");
  if (!card) return;
  const controls = Array.from(card.querySelectorAll<HTMLButtonElement | HTMLSelectElement>("button, select"));
  const rating = action === "rate" ? Number(card.querySelector<HTMLSelectElement>(".rec-rating")?.value) : undefined;
  controls.forEach(control => control.disabled = true);
  try {
    await api(`/api/v1/recommendations/${button.dataset.id}/actions`, {
      method: "POST",
      body: JSON.stringify({ action_id: crypto.randomUUID(), action, ...(rating ? { rating } : {}) })
    });
    const message = action === "request" ? "Added to Reelay" : action === "rate" ? `Rated ${rating} of 5` : "Recommendation dismissed";
    showToast(message);
    card.remove();
    const grid = document.querySelector<HTMLElement>(".recommendation-grid");
    if (grid && !grid.querySelector(".recommendation-card")) grid.innerHTML = `<div class="empty">No active recommendations</div>`;
  } catch (error) {
    controls.forEach(control => control.disabled = false);
    showError(error);
  }
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
  const [payload, profilesPayload, queuePayload] = await Promise.all([
    api<Item>(`/api/v1/series/${id}`), api<Item>("/api/v1/profiles"), api<Item>("/api/v1/queue")
  ]);
  const item = payload.series, episodes = payload.episodes ?? [];
  const profiles = profilesPayload.items ?? [];
  const episodeIDs = new Set<number>(episodes.map((episode: Item) => episode.id));
  const hasActive = (queuePayload.items ?? []).some((grab: Item) => grab.subject_type === "episode" && episodeIDs.has(grab.subject_id));
  const hasFiles = episodes.some((episode: Item) => Boolean(episode.imported_path));
  const detail = document.querySelector<HTMLElement>("#series-detail")!;
  detail.innerHTML = `<div class="section-head"><div><h2>${esc(item.title)}</h2><p>${esc(item.monitor_mode)} monitoring</p></div>
    <div class="row-actions"><button class="command" id="series-search">Search</button>
    <button class="command danger" id="series-delete">Delete</button></div></div>
    <div class="manage-bar"><label>Profile<select id="series-profile">${profileOptions(profiles, item.quality_profile_id)}</select></label>
    <label>Monitoring<select id="series-monitor">
      ${option("future_only", "Future only", item.monitor_mode)}${option("all", "All episodes", item.monitor_mode)}
      ${option("latest_season", "Latest season", item.monitor_mode)}${option("none", "None", item.monitor_mode)}</select></label>
    <label>Status<select id="series-status">${option("following", "Following", item.status)}
      ${option("paused", "Paused", item.status)}${option("ended", "Ended", item.status)}</select></label></div>
    ${table(["Episode", "Title", "Air date", "State", ""], episodes.map((e: Item) => [
      `S${pad(e.season)}E${pad(e.number)}`, esc(e.title || "Untitled"), date(e.air_date), state(e.state),
      `<button class="command compact episode-search" data-id="${e.id}">Search</button>`
    ]))}`;
  detail.querySelector<HTMLButtonElement>("#series-search")!.onclick = async () => {
    await api(`/api/v1/series/${id}/search`, { method: "POST" }); showToast("Series search started");
  };
  detail.querySelectorAll<HTMLButtonElement>(".episode-search").forEach(button => button.onclick = async () => {
    await api(`/api/v1/episodes/${button.dataset.id}/search`, { method: "POST" }); showToast("Episode search started");
  });
  detail.querySelector<HTMLSelectElement>("#series-profile")!.onchange = event =>
    patchSeries(id, { profile_id: Number((event.currentTarget as HTMLSelectElement).value) });
  detail.querySelector<HTMLSelectElement>("#series-monitor")!.onchange = event =>
    patchSeries(id, { monitor_mode: (event.currentTarget as HTMLSelectElement).value });
  detail.querySelector<HTMLSelectElement>("#series-status")!.onchange = event =>
    patchSeries(id, { status: (event.currentTarget as HTMLSelectElement).value });
  detail.querySelector<HTMLButtonElement>("#series-delete")!.onclick = () =>
    void deleteCollection("series", id, item.title, hasActive, hasFiles);
}

async function moviesView(): Promise<void> {
  const [payload, profilesPayload, queuePayload] = await Promise.all([
    api<Item>("/api/v1/movies"), api<Item>("/api/v1/profiles"), api<Item>("/api/v1/queue")
  ]);
  const items = payload.items ?? [];
  const profiles = profilesPayload.items ?? [];
  const activeByMovie = new Map<number, Item>((queuePayload.items ?? [])
    .filter((grab: Item) => grab.subject_type === "movie").map((grab: Item) => [grab.subject_id, grab]));
  const node = content(`<div class="page-head"><div><h1>Movies</h1><p>${items.length} tracked</p></div>
    <button class="command" data-view="add">+ <span>Add movie</span></button></div>
    <div class="collection-list">${items.length ? items.map((m: Item) => `<article class="collection-row">
      <div class="collection-title"><strong>${esc(m.title)}</strong><small>${esc(m.year)}</small></div>
      <div class="collection-field"><span>State</span>${state(m.state)}</div>
      <div class="collection-field"><span>Quality</span><strong>${esc(m.imported_quality || "Not imported")}</strong></div>
      <label class="collection-field">Profile<select class="table-select movie-profile" data-id="${m.id}">${profileOptions(profiles, m.quality_profile_id)}</select></label>
      <div class="row-actions"><button class="command compact movie-search" data-id="${m.id}">Search</button>
      ${activeByMovie.has(m.id) ? `<button class="command compact danger movie-cancel" data-id="${m.id}">Cancel</button>` : ""}
      <button class="command compact danger movie-delete" data-id="${m.id}">Delete</button></div></article>`).join("") : `<div class="empty">No tracked movies</div>`}</div>`);
  node.querySelector<HTMLElement>("[data-view=add]")!.onclick = () => navigate("add");
  node.querySelectorAll<HTMLButtonElement>(".movie-search").forEach(button => button.onclick = async () => {
    await api(`/api/v1/movies/${button.dataset.id}/search`, { method: "POST" }); showToast("Movie search started");
  });
  node.querySelectorAll<HTMLSelectElement>(".movie-profile").forEach(select => select.onchange = async () => {
    await api(`/api/v1/movies/${select.dataset.id}`, { method: "PATCH",
      body: JSON.stringify({ profile_id: Number(select.value) }) });
    showToast("Movie profile updated");
  });
  node.querySelectorAll<HTMLButtonElement>(".movie-cancel").forEach(button => button.onclick = () => {
    const grab = activeByMovie.get(Number(button.dataset.id));
    const movie = items.find((item: Item) => item.id === Number(button.dataset.id));
    if (grab && movie) void cancelGrab(grab, movie.title);
  });
  node.querySelectorAll<HTMLButtonElement>(".movie-delete").forEach(button => button.onclick = () => {
    const movie = items.find((item: Item) => item.id === Number(button.dataset.id));
    if (movie) void deleteCollection("movies", movie.id, movie.title,
      activeByMovie.has(movie.id), Boolean(movie.imported_path));
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
    <section><h2>Recommendations</h2>${table(["Setting", "Value"], [["Enabled", settings.recommendations.enabled ? "Yes" : "No"], ["Refresh", esc(settings.recommendations.refresh_interval)], ["Results per user", esc(settings.recommendations.result_limit)]])}</section>
    <section><h2>Manual triggers</h2><div class="trigger-row">${["search", "status", "metadata", "recent", "recommendations"].map(loop => `<button class="command trigger" data-loop="${loop}">↻ <span>${loop}</span></button>`).join("")}</div></section>`);
  node.querySelector<HTMLFormElement>("#token-form")!.onsubmit = event => {
	event.preventDefault();
	const form = event.currentTarget as HTMLFormElement;
	setAuthToken(form.querySelector("input")!.value); connect(); showToast("Token saved");
  };
  node.querySelectorAll<HTMLButtonElement>(".trigger").forEach(button => button.onclick = async () => {
    await api(`/api/v1/system/trigger/${button.dataset.loop}`, { method: "POST" }); showToast(`${button.dataset.loop} triggered`);
  });
}

function option(value: string, label: string, selected: string): string {
  return `<option value="${esc(value)}" ${value === selected ? "selected" : ""}>${esc(label)}</option>`;
}

function profileOptions(profiles: Item[], selected: number): string {
  return profiles.map(profile => option(String(profile.id), profile.name, String(selected))).join("");
}

async function patchSeries(id: number, body: Item): Promise<void> {
  try {
    await api(`/api/v1/series/${id}`, { method: "PATCH", body: JSON.stringify(body) });
    showToast("Series updated");
  } catch (error) {
    showError(error);
    await seriesDetail(id);
  }
}

function downloadLabel(grab: Item, movieNames: Map<number, string>): string {
  if (grab.subject_type === "movie" && movieNames.has(grab.subject_id)) return movieNames.get(grab.subject_id)!;
  const parts = String(grab.content_path || "").replaceAll("\\", "/").split("/").filter(Boolean);
  return parts.at(-1) || `${grab.subject_type} #${grab.subject_id}`;
}

async function cancelGrab(grab: Item, label: string): Promise<void> {
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
    await navigate(current);
  } catch (error) {
    showError(error);
  }
}

async function deleteCollection(kind: "movies" | "series", id: number, label: string,
  hasActive: boolean, hasFiles: boolean): Promise<void> {
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
    await navigate(kind === "movies" ? "movies" : "series");
  } catch (error) {
    showError(error);
  }
}

function confirmDialog(options: { title: string; message: string; confirmLabel: string; checks: DialogCheck[] }): Promise<Record<string, boolean> | null> {
  return new Promise(resolve => {
    const dialog = document.createElement("dialog");
    dialog.className = "confirm-dialog";
    dialog.innerHTML = `<form method="dialog"><header><h2>${esc(options.title)}</h2><p>${esc(options.message)}</p></header>
      <div class="dialog-options">${options.checks.map(check => `<label class="dialog-option">
        <input type="checkbox" name="${esc(check.id)}" ${check.checked ? "checked" : ""} ${check.required ? "required" : ""}>
        <span><strong>${esc(check.label)}</strong><small>${esc(check.detail)}</small></span></label>`).join("")}</div>
      <footer><button class="command" value="cancel" formnovalidate>Keep</button>
      <button class="command danger" value="confirm">${esc(options.confirmLabel)}</button></footer></form>`;
    document.body.append(dialog);
    dialog.addEventListener("close", () => {
      const confirmed = dialog.returnValue === "confirm";
      const values: Record<string, boolean> = {};
      options.checks.forEach(check => {
        values[check.id] = dialog.querySelector<HTMLInputElement>(`[name="${check.id}"]`)!.checked;
      });
      dialog.remove();
      resolve(confirmed ? values : null);
    }, { once: true });
    dialog.addEventListener("cancel", () => { dialog.returnValue = "cancel"; });
    dialog.showModal();
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
function connect(): void { eventSource?.close(); eventSource = connectEvents(() => { if (current === "discover") return; clearTimeout(refreshTimer); refreshTimer = window.setTimeout(() => navigate(current), 350); }); eventSource.onopen = () => setConnection("ok"); eventSource.onerror = () => setConnection("offline"); }

shell(); connect(); navigate("dashboard");
