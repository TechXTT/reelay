import "./styles.css";
import { api, APIError, authToken, connectEvents, esc, setAuthToken } from "./api.ts";

type View = "dashboard" | "discover" | "requests" | "series" | "movies" | "add" | "settings";
type Item = Record<string, any>;
type RequestMonitorMode = "latest_season" | "all" | "future_only";
type RecommendationRecord = {
  id: number;
  title: string;
  year?: number;
  overview?: string;
  poster_url?: string;
  score: number;
  vote_average: number;
  vote_count: number;
  genres?: string[];
  runtime_minutes?: number;
  reasons: string[];
  tmdb_id: number;
  generated_at: string;
  expires_at: string;
};
type PreviewRecord = {
  title: string;
  year: number;
  media_type: "movie" | "series";
  overview: string;
  poster_url: string;
  genres: string[];
  people: string[];
  runtime_minutes: number;
  vote_average: number;
  vote_count: number;
  videos: { name: string; key: string; type: string; official: boolean }[];
  seasons: number[];
};
type RequestRecord = {
  id: number;
  title: string;
  year: number;
  media_type: "movie" | "series";
  requested_at: string;
  monitor_mode: RequestMonitorMode | "";
  state: string;
  progress: number;
  last_error: string | null;
  next_search_at: string | null;
  available: boolean;
  imported_episodes: number;
  total_episodes: number;
  cancelled_at: string | null;
  seasons: number[];
  jellyfin_url: string;
};

const app = document.querySelector<HTMLDivElement>("#app")!;
let current: View = "dashboard";
let eventSource: EventSource | null = null;
let connectionStatus = "Connecting";
let refreshTimer = 0;
let refreshing = false;
let refreshPending = false;
let selectedSeries: number | null = null;
const discoverUserKey = "reelay.discover-user";
const discoverTypeKey = "reelay.discover-type";
let discoverUser = localStorage.getItem(discoverUserKey) ?? "";
let discoverType = localStorage.getItem(discoverTypeKey) === "series" ? "series" : "movie";
let requestsOffset = 0;
let attentionOnly = false;

type DialogCheck = { id: string; label: string; detail: string; checked?: boolean; required?: boolean };

const nav: { id: View; label: string; icon: string }[] = [
  { id: "dashboard", label: "Dashboard", icon: "◫" },
  { id: "discover", label: "Discover", icon: "*" },
  { id: "requests", label: "Requests", icon: "↧" },
  { id: "series", label: "Series", icon: "▤" },
  { id: "movies", label: "Movies", icon: "▶" },
  { id: "add", label: "Add", icon: "+" },
  { id: "settings", label: "Settings", icon: "⚙" }
];

function shell(): void {
  app.innerHTML = `<header class="topbar"><button class="brand" data-view="dashboard" aria-label="Dashboard">
    <span class="brand-mark">R</span><strong>Reelay</strong></button>
    <div id="connection" class="connection">Connecting</div></header>
    <div class="layout"><nav aria-label="Primary navigation">${nav.map(n => `<button data-view="${n.id}" title="${n.label}" aria-label="${n.label}" aria-current="${current === n.id ? "page" : "false"}" class="${current === n.id ? "active" : ""}">
      <span aria-hidden="true">${n.icon}</span><span>${n.label}</span></button>`).join("")}</nav>
    <main id="content"><div class="loading">Loading</div></main></div>
    <div id="toast" role="status" aria-live="polite"></div>`;
  document.querySelectorAll<HTMLElement>("[data-view]").forEach(el => el.onclick = () => navigate(el.dataset.view as View));
  setConnection(connectionStatus);
}

async function navigate(view: View): Promise<void> {
  current = view;
  selectedSeries = null;
  clearTimeout(refreshTimer);
  refreshTimer = 0;
  shell();
  try {
    if (view === "dashboard") await dashboard();
    if (view === "discover") await discoverView();
    if (view === "requests") await requestsView();
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
  if (error instanceof APIError && error.status === 401) {
    authGate(error.message);
    return;
  }
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
    api<Item>("/api/v1/health", {}, [503]), api<Item>("/api/v1/queue"), api<Item>("/api/v1/history?page=1"),
    api<Item>("/api/v1/movies")
  ]);
  if (current !== "dashboard" || (refreshing && isEditing())) return;
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
    const button = document.querySelector<HTMLButtonElement>("#refresh-search")!;
    await runControl(button, async () => {
      await api("/api/v1/system/trigger/search", { method: "POST" }); showToast("Search triggered");
    });
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
  if (current !== "discover") return;
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
  const values = (await api<{ items: RecommendationRecord[] }>(`/api/v1/recommendations?${query}`)).items;
  if (current !== "discover" || discoverUser !== key || discoverType !== mediaType) return;
  const node = content(`<div class="page-head"><div><h1>Discover</h1><p>Recommendations for ${esc(user.display_name)}</p></div>
    <div class="row-actions"><button class="command" id="discover-preferences">Preferences</button><button class="command" id="discover-history">Rating &amp; dismissal history</button><button class="command" id="generate-recommendations">Refresh</button></div></div>
    <div class="discover-toolbar"><label>User<select id="discover-user">${users.map((value: Item) => option(`${value.server_id}:${value.user_id}`, value.display_name, key)).join("")}</select></label>
    <div class="segmented"><label><input type="radio" name="discover-type" value="movie" ${mediaType === "movie" ? "checked" : ""}><span>Movies</span></label>
    <label><input type="radio" name="discover-type" value="series" ${mediaType === "series" ? "checked" : ""}><span>Series</span></label></div></div>
    <p class="discover-freshness">Jellyfin sync: ${user.last_synced_at ? esc(date(user.last_synced_at)) : "Not yet synchronized"}${values[0]?.generated_at ? ` · Recommendations generated ${esc(date(values[0].generated_at))}` : ""}${values[0]?.expires_at ? ` · Expires ${esc(date(values[0].expires_at))}` : ""}</p>
    <div class="recommendation-grid">${values.length ? values.map(item => `<article class="recommendation-card">
      ${item.poster_url ? `<img src="${esc(item.poster_url)}" alt="" loading="lazy">` : `<div class="recommendation-poster">${esc(item.title.charAt(0))}</div>`}
      <div class="recommendation-body"><div class="recommendation-title"><div><h2>${esc(item.title)}</h2><small>${esc(item.year || "Year unknown")}${item.runtime_minutes ? ` · ${item.runtime_minutes} min${mediaType === "series" ? " / episode" : ""}` : ""}</small></div><div class="match-score"><strong>${Number(item.score).toFixed(0)}</strong><small>Match score</small></div></div>
      <div class="audience-rating" data-audience-rating>${audienceRating(item.vote_average, item.vote_count)}</div>
      ${item.genres?.length ? `<small class="recommendation-genres">${esc(item.genres.join(" · "))}</small>` : ""}
      <p>${esc(item.overview || "No description available.")}</p><ol>${(item.reasons ?? []).map(reason => `<li>${esc(reason)}</li>`).join("")}</ol>
      <button class="command compact rec-preview" data-id="${item.id}" aria-label="Preview ${esc(item.title)}">Preview &amp; trailers</button>
      <div class="row-actions recommendation-actions"><button class="command compact rec-dismiss" data-id="${item.id}">Dismiss</button>
      <label class="rating-field"><span>Your rating</span><select class="rec-rating" aria-label="Your rating for ${esc(item.title)}"><option value="1">1</option><option value="2">2</option><option value="3">3</option><option value="4">4</option><option value="5" selected>5</option></select></label>
      <button class="command compact rec-rate" data-id="${item.id}">Rate</button>${mediaType === "series" ? `<label class="request-scope"><span>Episodes to request</span><select class="rec-monitor" aria-label="Episodes to request for ${esc(item.title)}">
        <option value="latest_season" selected>Latest season</option><option value="all">All episodes</option><option value="future_only">Future episodes</option><option value="specific">Specific seasons</option></select>
        <label class="specific-seasons" hidden>Season numbers (comma separated; 0 = specials)<input class="rec-seasons" inputmode="numeric" placeholder="1, 2" aria-label="Specific seasons for ${esc(item.title)}"></label>
        <small class="scope-explanation">Latest season requests the most recently aired season. All episodes includes past episodes; Future episodes follows upcoming air dates.</small></label>` : ""}<button class="command compact rec-request" data-id="${item.id}">Request</button></div></div>
    </article>`).join("") : `<div class="empty">No active recommendations</div>`}</div>`);
  node.querySelector<HTMLSelectElement>("#discover-user")!.onchange = event =>
    void discoverView((event.currentTarget as HTMLSelectElement).value, mediaType).catch(showError);
  node.querySelectorAll<HTMLInputElement>("[name=discover-type]").forEach(input => input.onchange = () =>
    void discoverView(key, input.value).catch(showError));
  node.querySelector<HTMLButtonElement>("#generate-recommendations")!.onclick = async () => {
    const button = node.querySelector<HTMLButtonElement>("#generate-recommendations")!;
    await runControl(button, async () => {
      await api("/api/v1/recommendations/generate", { method: "POST", body: JSON.stringify({ server_id: user.server_id, user_id: user.user_id, media_type: mediaType }) });
      showToast("Recommendations refreshed"); await discoverView(key, mediaType);
    });
  };
  for (const action of ["dismiss", "request"] as const) {
    node.querySelectorAll<HTMLButtonElement>(`.rec-${action}`).forEach(button => {
      button.onclick = () => void recommendationAction(button, action);
    });
  }
  node.querySelectorAll<HTMLButtonElement>(".rec-rate").forEach(button => {
    button.onclick = () => void recommendationAction(button, "rate");
  });
  node.querySelectorAll<HTMLButtonElement>(".rec-preview").forEach(button => {
    button.onclick = () => {
      const recommendation = values.find(value => value.id === Number(button.dataset.id))!;
      void recommendationPreview(button, recommendation);
    };
  });
  node.querySelectorAll<HTMLSelectElement>(".rec-monitor").forEach(select => select.onchange = () => {
    select.closest(".request-scope")!.querySelector<HTMLElement>(".specific-seasons")!.hidden = select.value !== "specific";
  });
  node.querySelector<HTMLButtonElement>("#discover-preferences")!.onclick = () => void discoverPreferences(user.server_id,user.user_id).catch(showError);
  node.querySelector<HTMLButtonElement>("#discover-history")!.onclick = () => void discoverHistory(user.server_id,user.user_id,mediaType).catch(showError);
}

async function discoverPreferences(serverID: string,userID: string) {
  const query = new URLSearchParams({server_id:serverID,user_id:userID});
  const preferences = await api<{languages:string[];excluded_genres:string[];familiarity:string;diversity:number}>(`/api/v1/recommendations/preferences?${query}`);
  const dialog = document.createElement("dialog");
  dialog.className = "preview-dialog";
  dialog.innerHTML = `<header class="preview-header"><h2>Recommendation preferences</h2><button class="command preferences-close" autofocus>Close</button></header>
    <form class="preferences-form"><label>Original languages (two-letter codes, comma separated)<input name="languages" value="${esc(preferences.languages.join(", "))}" placeholder="en, ja"></label>
    <label>Excluded genres (TMDB names, comma separated)<input name="genres" value="${esc(preferences.excluded_genres.join(", "))}" placeholder="Horror, Romance"></label>
    <label>Familiarity<select name="familiarity">${["balanced","familiar","explore"].map(value=>option(value,value,preferences.familiarity)).join("")}</select></label>
    <label>Diversity bonus (0–100%)<input name="diversity" type="number" min="0" max="100" value="${preferences.diversity}" required></label><p>Empty language and genre fields leave those choices unrestricted. Save, then Refresh Discover to apply.</p><button class="command" type="submit">Save preferences</button></form>`;
  document.body.append(dialog);
  dialog.querySelector<HTMLButtonElement>(".preferences-close")!.onclick = () => dialog.close();
  dialog.addEventListener("close",()=>dialog.remove(),{once:true});
  dialog.querySelector<HTMLFormElement>("form")!.onsubmit = event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget as HTMLFormElement);
    const split = (name:string) => String(form.get(name)).split(",").map(value=>value.trim()).filter(Boolean);
    void runControl(dialog.querySelector<HTMLButtonElement>("[type=submit]")!,async()=>{
      await api(`/api/v1/recommendations/preferences?${query}`,{method:"PUT",body:JSON.stringify({languages:split("languages"),excluded_genres:split("genres"),familiarity:form.get("familiarity"),diversity:Number(form.get("diversity"))})});
      dialog.close(); showToast("Preferences saved; refresh Discover to apply");
    });
  };
  dialog.showModal();
}

async function discoverHistory(serverID:string,userID:string,mediaType:string) {
  const query = new URLSearchParams({server_id:serverID,user_id:userID,media_type:mediaType});
  const history = await api<{items:RecommendationRecord[];ratings:{tmdb_id:number;rating:number}[]}>(`/api/v1/recommendations/history?${query}`);
  const dialog = document.createElement("dialog");
  dialog.className = "preview-dialog";
  dialog.innerHTML = `<header class="preview-header"><h2>Rating &amp; dismissal history</h2><button class="command history-close" autofocus>Close</button></header><p>Up to 100 dismissed titles; ratings remain part of your taste profile.</p>
    <div class="feedback-history">${history.items.map(recommendation=>{
      const rating = history.ratings.find(value=>value.tmdb_id===recommendation.tmdb_id)?.rating;
      return `<article><strong>${esc(recommendation.title)}</strong><div class="row-actions"><label>Your rating<select class="history-rating">${[1,2,3,4,5].map(value=>option(String(value),String(value),String(rating ?? 5))).join("")}</select></label><button class="command compact history-action" data-action="rate" data-id="${recommendation.id}">Save rating</button>${!rating ? `<button class="command compact history-action" data-action="undo" data-id="${recommendation.id}">Undo dismissal</button>`:""}</div></article>`;
    }).join("") || "No rating or dismissal history yet."}</div>`;
  document.body.append(dialog);
  dialog.querySelector<HTMLButtonElement>(".history-close")!.onclick = () => dialog.close();
  dialog.addEventListener("close",()=>dialog.remove(),{once:true});
  dialog.querySelectorAll<HTMLButtonElement>(".history-action").forEach(button=>button.onclick=()=>void runControl(button,async()=>{
    const rating = Number(button.closest("article")!.querySelector<HTMLSelectElement>("select")!.value);
    await api(`/api/v1/recommendations/${button.dataset.id}/actions`,{method:"POST",body:JSON.stringify({action_id:crypto.randomUUID(),action:button.dataset.action,...(button.dataset.action==="rate" ? {rating}:{})})});
    dialog.close(); showToast(button.dataset.action==="rate" ? "Rating updated":"Dismissal undone"); await discoverView();
  }));
  dialog.showModal();
}

function audienceRating(average: number, votes: number) {
  if (!votes) return "TMDB: rating unavailable";
  return `<strong>TMDB ${average.toFixed(1)}/10</strong><small>${votes.toLocaleString()} votes</small>`;
}

async function recommendationPreview(button: HTMLButtonElement, recommendation: RecommendationRecord) {
  const dialog = document.createElement("dialog");
  const controller = new AbortController();
  dialog.className = "preview-dialog";
  dialog.setAttribute("aria-labelledby", "preview-title");
  dialog.innerHTML = `<header class="preview-header"><h2 id="preview-title">${esc(recommendation.title)}</h2><button class="command compact preview-close" autofocus>Close</button></header>
    <div class="preview-content" aria-live="polite"><p>Loading preview…</p></div>`;
  document.body.append(dialog);
  dialog.querySelector<HTMLButtonElement>(".preview-close")!.onclick = () => dialog.close();
  dialog.addEventListener("close", () => {
    controller.abort();
    dialog.remove();
    if (button.isConnected) button.focus();
  }, { once: true });
  dialog.showModal();
  const previewContent = dialog.querySelector<HTMLElement>(".preview-content")!;
  const loadPreview = async () => {
    previewContent.innerHTML = `<p>Loading preview…</p>`;
    try {
      const payload = await api<unknown>(`/api/v1/recommendations/${recommendation.id}/preview`, { signal: controller.signal });
      if (controller.signal.aborted) return;
      // Validate the new API boundary without adding a runtime dependency.
      if (!payload || typeof payload !== "object") throw new Error("Invalid preview response");
      const preview = payload as PreviewRecord;
      if (typeof preview.title !== "string" || typeof preview.overview !== "string" || typeof preview.poster_url !== "string" ||
          !Number.isInteger(preview.year) || !Number.isInteger(preview.runtime_minutes) ||
          !Number.isFinite(preview.vote_average) || !Number.isInteger(preview.vote_count) ||
          (preview.media_type !== "movie" && preview.media_type !== "series") ||
          !Array.isArray(preview.genres) || !preview.genres.every(genre => typeof genre === "string") ||
          !Array.isArray(preview.people) || !preview.people.every(person => typeof person === "string") ||
          !Array.isArray(preview.videos) || !preview.videos.every(video => video && typeof video.name === "string" &&
            typeof video.key === "string" && /^[A-Za-z0-9_-]{11}$/.test(video.key) &&
            (video.type === "Trailer" || video.type === "Teaser") && typeof video.official === "boolean")) {
        throw new Error("Invalid preview response");
      }
      button.closest(".recommendation-card")!.querySelector("[data-audience-rating]")!.innerHTML = audienceRating(preview.vote_average, preview.vote_count);
      previewContent.innerHTML = `<div class="preview-summary">${preview.poster_url ? `<img src="${esc(preview.poster_url)}" alt="${esc(preview.title)} poster">` : ""}
        <div><p class="preview-facts">${preview.media_type === "series" ? "Series" : "Movie"} · ${preview.year || "Year unknown"}${preview.runtime_minutes ? ` · ${preview.runtime_minutes} min${preview.media_type === "series" ? " / episode" : ""}` : ""}</p>
        <div class="audience-rating">${audienceRating(preview.vote_average, preview.vote_count)}</div>
        ${preview.genres.length ? `<p>${esc(preview.genres.join(" · "))}</p>` : ""}${preview.media_type === "series" && preview.seasons?.length ? `<p>Known seasons: ${esc(preview.seasons.join(", "))} (0 = specials)</p>` : ""}<p class="preview-overview">${esc(preview.overview || "No description available.")}</p>
        ${preview.people.length ? `<p class="preview-people"><strong>Cast &amp; filmmakers</strong><br>${esc(preview.people.join(", "))}</p>` : ""}</div></div>
        <section class="preview-trailers"><h3>Trailers &amp; teasers</h3>${preview.videos.length ? `<label>Video<select class="preview-video">${preview.videos.map((video, index) => `<option value="${index}">${esc(video.name)} (${video.official ? "Official " : ""}${esc(video.type)})</option>`).join("")}</select></label>
          <div class="preview-player"></div><div class="preview-video-actions"><button class="command preview-play">Play preview</button><a class="preview-external" target="_blank" rel="noopener noreferrer">Watch on YouTube</a></div><p>Video playback is provided by YouTube.</p>` : `<p>No trailer or teaser is available for this title.</p>`}</section>`;
      if (!preview.videos.length) return;
      const videoSelect = previewContent.querySelector<HTMLSelectElement>(".preview-video")!;
      const external = previewContent.querySelector<HTMLAnchorElement>(".preview-external")!;
      const player = previewContent.querySelector<HTMLElement>(".preview-player")!;
      const play = previewContent.querySelector<HTMLButtonElement>(".preview-play")!;
      external.href = `https://www.youtube.com/watch?v=${preview.videos[0].key}`;
      videoSelect.onchange = () => {
        player.replaceChildren();
        play.hidden = false;
        external.href = `https://www.youtube.com/watch?v=${preview.videos[Number(videoSelect.value)].key}`;
      };
      play.onclick = () => {
        const video = preview.videos[Number(videoSelect.value)];
        player.innerHTML = `<iframe src="https://www.youtube-nocookie.com/embed/${video.key}" title="${esc(video.name)}" allow="encrypted-media; picture-in-picture; fullscreen" referrerpolicy="strict-origin-when-cross-origin" allowfullscreen></iframe>`;
        play.hidden = true;
      };
    } catch (error) {
      if (controller.signal.aborted) return;
      previewContent.innerHTML = `<p class="preview-overview">${esc(recommendation.overview || "No description available.")}</p><p class="preview-error" role="alert">${esc(error instanceof Error ? error.message : String(error))}</p><button class="command preview-retry">Retry preview</button>`;
      previewContent.querySelector<HTMLButtonElement>(".preview-retry")!.onclick = () => void loadPreview();
    }
  };
  await loadPreview();
}

async function recommendationAction(button: HTMLButtonElement, action: "dismiss" | "request" | "rate"): Promise<void> {
  const card = button.closest<HTMLElement>(".recommendation-card");
  if (!card) return;
  const controls = Array.from(card.querySelectorAll<HTMLButtonElement | HTMLSelectElement>("button, select"));
  const rating = action === "rate" ? Number(card.querySelector<HTMLSelectElement>(".rec-rating")?.value) : undefined;
  const monitorMode = action === "request" ? card.querySelector<HTMLSelectElement>(".rec-monitor")?.value as RequestMonitorMode | undefined : undefined;
  const seasonScope = card.querySelector<HTMLSelectElement>(".rec-monitor")?.value === "specific";
  const seasons = seasonScope ? card.querySelector<HTMLInputElement>(".rec-seasons")!.value.split(",").map(value => /^\d+$/.test(value.trim()) ? Number(value.trim()) : NaN) : [];
  if (action === "request" && seasonScope && (!card.querySelector<HTMLInputElement>(".rec-seasons")!.value.trim() || seasons.length > 100 || seasons.some(value => !Number.isInteger(value) || value < 0 || value > 999))) {
    showError(new Error("Enter season numbers between 0 and 999, separated by commas.")); return;
  }
  controls.forEach(control => control.disabled = true);
  try {
    await api(`/api/v1/recommendations/${button.dataset.id}/actions`, {
      method: "POST",
      body: JSON.stringify({ action_id: crypto.randomUUID(), action, ...(rating ? { rating } : {}), ...(action === "request" && seasonScope ? { seasons } : monitorMode ? { monitor_mode: monitorMode } : {}) })
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

async function requestsView(selectedUser = discoverUser): Promise<void> {
  const offset = requestsOffset;
  const attention = attentionOnly;
  const users = (await api<Item>("/api/v1/integrations/jellyfin/users")).items ?? [];
  if (current !== "requests") return;
  if (!users.length) {
    content(`<div class="page-head"><div><h1>Requests</h1><p>Track recommendations through library availability</p></div></div>
      <div class="empty">Install and configure the Reelay Jellyfin plugin to view requests by user.</div>`);
    return;
  }
  const user = users.find((value: Item) => `${value.server_id}:${value.user_id}` === selectedUser) ?? users[0];
  const key = `${user.server_id}:${user.user_id}`;
  discoverUser = key;
  localStorage.setItem(discoverUserKey, key);
  const query = `server_id=${encodeURIComponent(user.server_id)}&user_id=${encodeURIComponent(user.user_id)}&offset=${offset}&attention=${attention}`;
  const requestPayload = await api<{items:RequestRecord[];has_more:boolean}>(`/api/v1/requests?${query}`);
  const records = requestPayload.items;
  if (current !== "requests" || discoverUser !== key || requestsOffset !== offset || attentionOnly !== attention) return;
  const node = content(`<div class="page-head requests-head"><div><h1>Recent requests</h1><p>Most recent requests for ${esc(user.display_name)} · follow each title until it is available in Jellyfin</p></div>
    <div class="row-actions"><label class="request-user">Jellyfin user<select id="requests-user">${users.map((value: Item) => option(`${value.server_id}:${value.user_id}`, value.display_name, key)).join("")}</select></label>
    <button class="command" id="refresh-requests" aria-label="Refresh requests">↻ <span>Refresh</span></button></div></div>
    <div class="row-actions request-filters"><button class="command requests-attention" aria-pressed="${attentionOnly}">${attentionOnly ? "Show all requests" : "Attention needed"}</button><button class="command requests-previous" ${requestsOffset===0 ? "disabled" : ""}>Newer</button><button class="command requests-next" ${!requestPayload.has_more ? "disabled" : ""}>Older</button></div>
    <div class="request-list">${records.length ? records.map(record => `<article class="request-row">
      <div class="request-title"><strong>${esc(record.title)}</strong><small>${esc(record.media_type)}${record.year ? ` · ${esc(record.year)}` : ""}</small></div>
      <div class="request-status"><span>Status</span>${state(record.state)}${record.available ? `<span class="state state-available">Available in Jellyfin</span>` : ""}</div>
      <div class="request-progress"><span>Progress</span>${record.media_type === "series" && record.total_episodes > 0 ? `<strong>${record.imported_episodes} of ${record.total_episodes} known episodes</strong>` : `<strong>${Math.max(0, Math.min(100, record.progress * 100)).toFixed(0)}%</strong>`}
        ${record.progress > 0 && !record.available ? `<div class="progress"><i style="width:${Math.max(0, Math.min(100, record.progress * 100))}%"></i></div>` : ""}</div>
      <div class="request-detail"><span>Requested</span><strong>${date(record.requested_at)}</strong></div>
      ${record.media_type === "series" && record.monitor_mode ? `<div class="request-detail"><span>Requested scope</span><strong>${esc(monitorLabel(record.monitor_mode))}</strong></div>` : ""}
      ${record.last_error ? `<p class="request-error">${esc(record.last_error)}</p>` : ""}
      ${record.next_search_at ? `<small class="request-next">Next search ${esc(date(record.next_search_at))}</small>` : ""}
      ${record.seasons?.length ? `<small class="request-next">Requested seasons: ${esc(record.seasons.join(", "))}</small>` : ""}
      <div class="row-actions request-controls">${record.jellyfin_url ? `<a class="preview-external" href="${esc(record.jellyfin_url)}" target="_blank" rel="noopener noreferrer">Open in Jellyfin</a>` : ""}
      ${(record.media_type === "series" || !record.available) && ["wanted","failed","import_failed","attention_needed","searching","cancelled"].includes(record.state) ? `<button class="command compact request-action" data-id="${record.id}" data-action="retry">Retry</button>` : ""}
      ${!record.cancelled_at ? `<button class="command compact request-action" data-id="${record.id}" data-action="cancel">Withdraw request</button>` : ""}<button class="command compact request-details" data-id="${record.id}">Diagnostics</button></div>
    </article>`).join("") : `<div class="empty">No requests for ${esc(user.display_name)} yet. Request a recommendation from Discover to track it here.</div>`}</div>`);
  node.querySelector<HTMLSelectElement>("#requests-user")!.onchange = event => {
    requestsOffset=0; void requestsView((event.currentTarget as HTMLSelectElement).value).catch(showError);
  };
  node.querySelector<HTMLButtonElement>("#refresh-requests")!.onclick = () =>
    void runControl(node.querySelector<HTMLButtonElement>("#refresh-requests")!, () => requestsView(key));
  node.querySelectorAll<HTMLButtonElement>(".request-action").forEach(button => button.onclick = () => void runControl(button,async () => {
    await api(`/api/v1/requests/${button.dataset.id}/actions`, {method:"POST",body:JSON.stringify({action:button.dataset.action})});
    showToast(button.dataset.action === "cancel" ? "Request withdrawn; shared downloads continue" : "Retry scheduled");
    await requestsView(key);
  }));
  node.querySelectorAll<HTMLButtonElement>(".request-details").forEach(button => button.onclick = () => void requestDiagnostics(Number(button.dataset.id)).catch(showError));
  node.querySelector<HTMLButtonElement>(".requests-attention")!.onclick = () => { attentionOnly=!attentionOnly; requestsOffset=0; void requestsView(key).catch(showError); };
  node.querySelector<HTMLButtonElement>(".requests-previous")!.onclick = () => { requestsOffset=Math.max(0,requestsOffset-100); void requestsView(key).catch(showError); };
  node.querySelector<HTMLButtonElement>(".requests-next")!.onclick = () => { requestsOffset+=100; void requestsView(key).catch(showError); };
}

function monitorLabel(mode: RequestMonitorMode): string {
  if (mode === "latest_season") return "Latest season";
  if (mode === "all") return "All episodes";
  return "Future episodes";
}

async function requestDiagnostics(id: number) {
  const payload = await api<{ subjects: { type: string; id: number; title: string; state: string; error: string; search_attempts: number; next_search_at?: string; last_search_at?: string; history: { reason: string; detail?: string; at: string }[]; candidates: { evaluation: { accepted: boolean; reason: string; score: number }; release: { id: number; raw_title: string } }[] }[] }>(`/api/v1/requests/${id}/diagnostics`);
  const dialog = document.createElement("dialog");
  dialog.className = "preview-dialog";
  dialog.innerHTML = `<header class="preview-header"><h2>Request diagnostics</h2><button class="command diagnostics-close" autofocus>Close</button></header><p>Latest persisted history and evaluations. Up to 50 subjects and 100 candidates per subject.</p>
    ${payload.subjects.map(subject => `<section><h3>${esc(subject.title || `${subject.type} #${subject.id}`)}</h3>${state(subject.state)}${subject.error ? `<p class="preview-error">${esc(subject.error)}</p>` : ""}
    <p>Search attempts: ${esc(subject.search_attempts ?? 0)} · Last search: ${subject.last_search_at ? esc(date(subject.last_search_at)) : "Not in recent history"}${subject.next_search_at ? ` · Next retry: ${esc(date(subject.next_search_at))}` : ""}</p>
    <ol>${(subject.history ?? []).map(transition => `<li>${date(transition.at)}: ${esc(transition.reason)} ${esc(transition.detail || "")}</li>`).join("")}</ol>
    <div class="diagnostic-candidates">${subject.candidates.map(candidate => `<article><strong>${esc(candidate.release.raw_title)}</strong><p>${candidate.evaluation.accepted ? `Accepted · score ${candidate.evaluation.score}` : "Rejected"}: ${esc(candidate.evaluation.reason)}</p>${candidate.evaluation.accepted ? `<button class="command compact candidate-grab" data-subject="${subject.id}" data-type="${subject.type}" data-release="${candidate.release.id}">Select release</button>` : ""}</article>`).join("") || "No candidates persisted yet."}</div></section>`).join("") || `<p>No active episode diagnostics are available.</p>`}`;
  document.body.append(dialog);
  dialog.querySelector<HTMLButtonElement>(".diagnostics-close")!.onclick = () => dialog.close();
  dialog.addEventListener("close",() => dialog.remove(),{once:true});
  dialog.querySelectorAll<HTMLButtonElement>(".candidate-grab").forEach(button => button.onclick = () => void runControl(button,async () => {
    await api(`/api/v1/requests/${id}/grab`,{method:"POST",body:JSON.stringify({subject_type:button.dataset.type,subject_id:Number(button.dataset.subject),release_id:Number(button.dataset.release)})});
    dialog.close(); showToast("Release selected"); await requestsView();
  }));
  dialog.showModal();
}

async function runControl(control: HTMLButtonElement | HTMLSelectElement, action: () => Promise<void>,
  recover?: () => void, keepDisabledOnSuccess = false): Promise<void> {
  if (control.disabled) return;
  control.disabled = true;
  let succeeded = false;
  try {
    await action();
    succeeded = true;
  } catch (error) {
    showError(error);
    recover?.();
  } finally {
    if (control.isConnected && !(succeeded && keepDisabledOnSuccess)) control.disabled = false;
  }
}

async function seriesView(): Promise<void> {
  const payload = await api<Item>("/api/v1/series");
  if (current !== "series" || (refreshing && isEditing())) return;
  const items = payload.items ?? [];
  const node = content(`<div class="page-head"><div><h1>Series</h1><p>${items.length} followed</p></div>
    <button class="command" data-view="add">+ <span>Add series</span></button></div>
    <div class="media-grid">${items.length ? items.map((item: Item) => `<button class="media-card" data-series="${item.id}">
      <span class="media-monogram">${esc(item.title.charAt(0))}</span><div><strong>${esc(item.title)}</strong>
      <small>${esc(item.year || "Year unknown")} · ${esc(item.monitor_mode)}</small></div>${state(item.status)}</button>`).join("") : `<div class="empty">No followed series</div>`}</div>
    <section id="series-detail"></section>`);
  node.querySelector<HTMLElement>("[data-view=add]")!.onclick = () => navigate("add");
  node.querySelectorAll<HTMLElement>("[data-series]").forEach(el => el.onclick = () => void seriesDetail(Number(el.dataset.series)).catch(showError));
}

async function seriesDetail(id: number): Promise<void> {
  selectedSeries = id;
  const [payload, profilesPayload, queuePayload] = await Promise.all([
    api<Item>(`/api/v1/series/${id}`), api<Item>("/api/v1/profiles"), api<Item>("/api/v1/queue")
  ]);
  if (current !== "series" || selectedSeries !== id || (refreshing && isEditing())) return;
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
    const button = detail.querySelector<HTMLButtonElement>("#series-search")!;
    await runControl(button, async () => {
      await api(`/api/v1/series/${id}/search`, { method: "POST" }); showToast("Series search started");
    });
  };
  detail.querySelectorAll<HTMLButtonElement>(".episode-search").forEach(button => button.onclick = async () => {
    await runControl(button, async () => {
      await api(`/api/v1/episodes/${button.dataset.id}/search`, { method: "POST" }); showToast("Episode search started");
    });
  });
  detail.querySelectorAll<HTMLSelectElement>("#series-profile, #series-monitor, #series-status").forEach(select => {
    select.onchange = () => void runControl(select, async () => {
      const value = select.value;
      if (select.id === "series-profile") await patchSeries(item, { profile_id: Number(value) });
      if (select.id === "series-monitor") await patchSeries(item, { monitor_mode: value });
      if (select.id === "series-status") await patchSeries(item, { status: value });
    }, () => {
      if (select.id === "series-profile") select.value = String(item.quality_profile_id);
      if (select.id === "series-monitor") select.value = String(item.monitor_mode);
      if (select.id === "series-status") select.value = String(item.status);
    });
  });
  detail.querySelector<HTMLButtonElement>("#series-delete")!.onclick = () =>
    void deleteCollection("series", id, item.title, hasActive, hasFiles);
}

async function moviesView(): Promise<void> {
  const [payload, profilesPayload, queuePayload] = await Promise.all([
    api<Item>("/api/v1/movies"), api<Item>("/api/v1/profiles"), api<Item>("/api/v1/queue")
  ]);
  if (current !== "movies" || (refreshing && isEditing())) return;
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
    await runControl(button, async () => {
      await api(`/api/v1/movies/${button.dataset.id}/search`, { method: "POST" }); showToast("Movie search started");
    });
  });
  node.querySelectorAll<HTMLSelectElement>(".movie-profile").forEach(select => select.onchange = async () => {
    const movie = items.find((item: Item) => item.id === Number(select.dataset.id));
    if (!movie) return;
    await runControl(select, async () => {
      const profileID = Number(select.value);
      await api(`/api/v1/movies/${select.dataset.id}`, { method: "PATCH",
        body: JSON.stringify({ profile_id: profileID }) });
      movie.quality_profile_id = profileID;
      showToast("Movie profile updated");
    }, () => { select.value = String(movie.quality_profile_id); });
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
  if (current !== "add") return;
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
    const searchButton = form.querySelector<HTMLButtonElement>('button[type="submit"]')!;
    await runControl(searchButton, async () => {
      const payload = await api<Item>(`/api/v1/metadata/search?type=${type}&q=${encodeURIComponent(query)}`);
      if (current !== "add") return;
      const results = node.querySelector<HTMLElement>("#search-results")!;
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
        await runControl(button, async () => {
          await api(`/api/v1/${type === "series" ? "series" : "movies"}`, { method: "POST", body: JSON.stringify(body) });
          button.textContent = "Added";
          showToast(`${result.title} added`);
        }, undefined, true);
      });
    });
  };
}

async function settingsView(): Promise<void> {
  const [settings, profiles] = await Promise.all([api<Item>("/api/v1/settings"), api<Item>("/api/v1/profiles")]);
  if (current !== "settings") return;
  const node = content(`<div class="page-head"><div><h1>Settings</h1><p>Runtime configuration</p></div><div class="row-actions"><button class="command" id="setup-checks">Setup checks</button><button class="command" id="database-backup">Download database backup</button></div></div>
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
    await runControl(button, async () => {
      await api(`/api/v1/system/trigger/${button.dataset.loop}`, { method: "POST" }); showToast(`${button.dataset.loop} triggered`);
    });
  });
  node.querySelector<HTMLButtonElement>("#setup-checks")!.onclick = () => void runControl(node.querySelector<HTMLButtonElement>("#setup-checks")!,async()=>{
    const setup = await api<{checks:{name:string;status:string;detail?:string;action:string;available_bytes?:number}[];webhook_enabled:boolean}>("/api/v1/setup");
    const dialog = document.createElement("dialog");
    dialog.className = "preview-dialog";
    dialog.innerHTML = `<header class="preview-header"><h2>Setup checks</h2><button class="command setup-close" autofocus>Close</button></header><p>Availability webhook: ${setup.webhook_enabled ? "Enabled":"Not configured"}</p><ol class="setup-checks">${setup.checks.map(check=>`<li><strong>${esc(check.name)}</strong> ${state(check.status)}<p>${esc(check.detail || "")}${check.available_bytes!==undefined ? ` ${(check.available_bytes/1024**3).toFixed(1)} GiB available`:""}</p><p>${esc(check.action)}</p></li>`).join("")}</ol>`;
    document.body.append(dialog);
    dialog.querySelector<HTMLButtonElement>(".setup-close")!.onclick = () => dialog.close();
    dialog.addEventListener("close",()=>dialog.remove(),{once:true}); dialog.showModal();
  });
  node.querySelector<HTMLButtonElement>("#database-backup")!.onclick = () => void runControl(node.querySelector<HTMLButtonElement>("#database-backup")!,async()=>{
    const headers = new Headers();
    if (authToken()) headers.set("Authorization",`Bearer ${authToken()}`);
    const response = await fetch("/api/v1/database/backup",{method:"POST",headers});
    if (!response.ok) throw new APIError(`Database backup failed (${response.status})`,response.status,"backup_failed");
    const download = document.createElement("a");
    const objectURL = URL.createObjectURL(await response.blob());
    download.href=objectURL; download.download="reelay-backup.db"; download.click();
    window.setTimeout(()=>URL.revokeObjectURL(objectURL),1000);
    showToast("Database backup downloaded");
  });
}

function option(value: string, label: string, selected: string): string {
  return `<option value="${esc(value)}" ${value === selected ? "selected" : ""}>${esc(label)}</option>`;
}

function profileOptions(profiles: Item[], selected: number): string {
  return profiles.map(profile => option(String(profile.id), profile.name, String(selected))).join("");
}

async function patchSeries(item: Item, body: Item): Promise<void> {
  await api(`/api/v1/series/${item.id}`, { method: "PATCH", body: JSON.stringify(body) });
  if (body.profile_id !== undefined) item.quality_profile_id = body.profile_id;
  if (body.monitor_mode !== undefined) item.monitor_mode = body.monitor_mode;
  if (body.status !== undefined) item.status = body.status;
  showToast("Series updated");
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
const dateFormatter = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });
const dateTimeFormatter = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
const date = (value: string) => value
  ? (value.includes("T") ? dateTimeFormatter : dateFormatter).format(new Date(value))
  : "—";
function showToast(message: string): void { const toast = document.querySelector<HTMLElement>("#toast")!; toast.textContent = message; toast.className = "show"; setTimeout(() => toast.className = "", 3000); }
function setConnection(status: string): void { connectionStatus = status; const el = document.querySelector<HTMLElement>("#connection"); if (el) { el.textContent = status === "ok" ? "Connected" : status; el.className = `connection ${status}`; } }
function isEditing() {
  return Boolean(document.querySelector("dialog[open]") ||
    document.activeElement?.matches("input, select, textarea"));
}

async function refreshLiveView() {
  if (!["dashboard", "requests", "series", "movies"].includes(current) || isEditing()) return;
  if (refreshing) {
    refreshPending = true;
    return;
  }
  refreshing = true;
  try {
    if (current === "dashboard") await dashboard();
    else if (current === "requests") await requestsView();
    else if (current === "movies") await moviesView();
    else if (selectedSeries === null) await seriesView();
    else await seriesDetail(selectedSeries);
  } catch (error) {
    if (error instanceof APIError && error.status === 401) authGate(error.message);
    else showError(error);
  } finally {
    refreshing = false;
    if (refreshPending) {
      refreshPending = false;
      void refreshLiveView();
    }
  }
}

function connect() {
  eventSource?.close();
  eventSource = connectEvents(() => {
    if (refreshTimer) return;
    refreshTimer = window.setTimeout(() => {
      refreshTimer = 0;
      void refreshLiveView();
    }, 350);
  });
  eventSource.onopen = () => {
    setConnection("ok");
    void refreshLiveView();
  };
  eventSource.onerror = () => setConnection("offline");
}

shell(); connect(); navigate("dashboard");
