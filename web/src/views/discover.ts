import { api, esc } from "../api.ts";
import { discoverTypeKey, discoverUserKey, hooks, session } from "../session.ts";
import type {
  ItemList, JellyfinUser, MediaType, Recommendation, RecommendationAction, RecommendationHistoryResponse,
  RecommendationPreferences, RecommendationPreview, RequestMonitorMode, Trial, TrialDecision, TrialScope
} from "../types.ts";
import {
  bindAll, bindRequest, content, date, jsonRequest, openDialog, option, pick, runControl, showError, showToast,
  userKey, userOptions
} from "../ui.ts";

const trialScopes: string[] = ["one_episode", "three_episodes", "first_season"];

export function trialLabel(scope: TrialScope) {
  return scope === "one_episode" ? "1-episode trial" : scope === "three_episodes" ? "3-episode trial" : "First-season trial";
}

function audienceRating(average: number, votes: number) {
  if (!votes) return "TMDB: rating unavailable";
  return `<strong>TMDB ${average.toFixed(1)}/10</strong><small>${votes.toLocaleString()} votes</small>`;
}

function discoverHead(user: JellyfinUser, users: JellyfinUser[], key: string, mediaType: MediaType, recommendations: Recommendation[]) {
  return `<div class="page-head"><div><h1>Discover</h1><p>Recommendations for ${esc(user.display_name)}</p></div>
    <div class="row-actions"><button class="command" id="discover-preferences">Preferences</button><button class="command" id="discover-history">Rating &amp; dismissal history</button><button class="command" id="generate-recommendations">Refresh</button></div></div>
    <div class="discover-toolbar"><label>User<select id="discover-user">${userOptions(users, key)}</select></label>
    <div class="segmented"><label><input type="radio" name="discover-type" value="movie" ${mediaType === "movie" ? "checked" : ""}><span>Movies</span></label>
    <label><input type="radio" name="discover-type" value="series" ${mediaType === "series" ? "checked" : ""}><span>Series</span></label></div></div>
    <p class="discover-freshness">Jellyfin sync: ${user.last_synced_at ? esc(date(user.last_synced_at)) : "Not yet synchronized"}${recommendations[0]?.generated_at ? ` · Recommendations generated ${esc(date(recommendations[0].generated_at))}` : ""}${recommendations[0]?.expires_at ? ` · Expires ${esc(date(recommendations[0].expires_at))}` : ""}</p>`;
}

function trialCard(trial: Trial) {
  const awaitingVote = trial.state === "awaiting_vote";
  return `<article class="trial-card" data-trial-id="${trial.request_id}"><div><h3>${esc(trial.title)}</h3><small>${esc(trialLabel(trial.scope))} · ${trial.episode_limit ? `${trial.watched_episodes} of ${trial.episode_limit} watched` : "Waiting for episode metadata"}</small></div>
        <p>${awaitingVote ? "Trial finished — your vote is required before continuing." : "Further episodes stay on hold until your vote. Jellyfin watch progress updates about once a minute."}</p>
        <div class="row-actions"><label>Your rating<select class="trial-rating" aria-label="Trial rating for ${esc(trial.title)}"><option value="" selected>Choose 1–5</option>${[1,2,3,4,5].map(value=>`<option value="${value}">${value}</option>`).join("")}</select></label>
        <button class="command trial-vote" data-decision="continue" ${!awaitingVote ? "disabled" : ""}>Continue series</button><button class="command trial-vote" data-decision="stop" ${!awaitingVote ? "disabled" : ""}>Stop here</button><button class="command trial-requests">View requests</button></div></article>`;
}

function trialsSection(trials: Trial[]) {
  if (!trials.length) return "";
  return `<section class="series-trials" aria-labelledby="trials-heading"><h2 id="trials-heading">Your series trials</h2>
      <p>Watch your trial, then rate it and choose Continue or Stop. Continue requests the rest of the series. Another user's broader requests keep running.</p>
      ${trials.map(trialCard).join("")}</section>`;
}

function seriesScopeControls(title: string) {
  return `<label class="request-scope"><span>Episodes to request</span><select class="rec-monitor" aria-label="Episodes to request for ${esc(title)}">
        <option value="latest_season" selected>Latest season</option><option value="all">All episodes</option><option value="future_only">Future episodes</option><option value="specific">Specific seasons</option><option value="one_episode">Try 1 episode</option><option value="three_episodes">Try 3 episodes</option><option value="first_season">Try the first season</option></select>
        <label class="specific-seasons" hidden>Season numbers (comma separated; 0 = specials)<input class="rec-seasons" inputmode="numeric" placeholder="1, 2" aria-label="Specific seasons for ${esc(title)}"></label>
        <small class="scope-explanation">Trials start from the beginning and skip specials. Short series use up to 3 known episodes; a season trial follows all known episodes of the first season. A Continue/Stop vote and personal rating are required afterward.</small></label>`;
}

function recommendationCard(item: Recommendation, mediaType: MediaType) {
  return `<article class="recommendation-card">
      ${item.poster_url ? `<img src="${esc(item.poster_url)}" alt="" loading="lazy">` : `<div class="recommendation-poster">${esc(item.title.charAt(0))}</div>`}
      <div class="recommendation-body"><div class="recommendation-title"><div><h2>${esc(item.title)}</h2><small>${esc(item.year || "Year unknown")}${item.runtime_minutes ? ` · ${item.runtime_minutes} min${mediaType === "series" ? " / episode" : ""}` : ""}</small></div><div class="match-score"><strong>${Number(item.score).toFixed(0)}</strong><small>Match score</small></div></div>
      <div class="audience-rating" data-audience-rating>${audienceRating(item.vote_average, item.vote_count)}</div>
      ${item.genres?.length ? `<small class="recommendation-genres">${esc(item.genres.join(" · "))}</small>` : ""}
      <p>${esc(item.overview || "No description available.")}</p><ol>${(item.reasons ?? []).map(reason => `<li>${esc(reason)}</li>`).join("")}</ol>
      <button class="command compact rec-preview" data-id="${item.id}" aria-label="Preview ${esc(item.title)}">Preview &amp; trailers</button>
      <div class="row-actions recommendation-actions"><button class="command compact rec-dismiss" data-id="${item.id}">Dismiss</button>
      <label class="rating-field"><span>Your rating</span><select class="rec-rating" aria-label="Your rating for ${esc(item.title)}"><option value="1">1</option><option value="2">2</option><option value="3">3</option><option value="4">4</option><option value="5" selected>5</option></select></label>
      <button class="command compact rec-rate" data-id="${item.id}">Rate</button>${mediaType === "series" ? seriesScopeControls(item.title) : ""}<button class="command compact rec-request" data-id="${item.id}">Request</button></div></div>
    </article>`;
}

export async function discoverView(selectedUser = session.discoverUser, mediaType = session.discoverType) {
  const users = (await api<ItemList<JellyfinUser>>("/api/v1/integrations/jellyfin/users")).items ?? [];
  if (session.current !== "discover") return;
  if (!users.length) {
    content(`<div class="page-head"><div><h1>Discover</h1><p>Personalized from Jellyfin activity</p></div></div>
      <div class="empty">Install and configure the Reelay Jellyfin plugin to synchronize users.</div>`);
    return;
  }
  const user = users.find(value => userKey(value) === selectedUser) ?? users[0];
  const key = userKey(user);
  session.discoverUser = key;
  session.discoverType = mediaType;
  localStorage.setItem(discoverUserKey, key);
  localStorage.setItem(discoverTypeKey, mediaType);
  const query = `server_id=${encodeURIComponent(user.server_id)}&user_id=${encodeURIComponent(user.user_id)}&media_type=${mediaType}`;
  const [recommendationPage, trialPage] = await Promise.all([
    api<ItemList<Recommendation>>(`/api/v1/recommendations?${query}`),
    api<{ items: Trial[] }>(`/api/v1/trials?server_id=${encodeURIComponent(user.server_id)}&user_id=${encodeURIComponent(user.user_id)}`)
  ]);
  const recommendations = recommendationPage.items ?? [];
  if (session.current !== "discover" || session.discoverUser !== key || session.discoverType !== mediaType) return;
  const node = content(`${discoverHead(user, users, key, mediaType, recommendations)}
    ${trialsSection(trialPage.items)}
    <div class="recommendation-grid">${recommendations.length ? recommendations.map(item => recommendationCard(item, mediaType)).join("") : `<div class="empty">No active recommendations</div>`}</div>`);
  bindDiscoverToolbar(node, user, key, mediaType);
  bindTrialControls(node, key, mediaType);
  bindRecommendationCards(node, recommendations);
}

function bindDiscoverToolbar(node: HTMLElement, user: JellyfinUser, key: string, mediaType: MediaType) {
  pick<HTMLSelectElement>(node, "#discover-user").onchange = event =>
    void discoverView((event.currentTarget as HTMLSelectElement).value, mediaType).catch(showError);
  bindAll<HTMLInputElement>(node, "[name=discover-type]", input => input.onchange = () =>
    void discoverView(key, input.value as MediaType).catch(showError));
  bindRequest(pick<HTMLButtonElement>(node, "#generate-recommendations"), "/api/v1/recommendations/generate",
    jsonRequest("POST", { server_id: user.server_id, user_id: user.user_id, media_type: mediaType }),
    "Recommendations refreshed", () => discoverView(key, mediaType));
  pick<HTMLButtonElement>(node, "#discover-preferences").onclick = () =>
    void discoverPreferences(user.server_id, user.user_id).catch(showError);
  pick<HTMLButtonElement>(node, "#discover-history").onclick = () =>
    void discoverHistory(user.server_id, user.user_id, mediaType).catch(showError);
}

function bindTrialControls(node: HTMLElement, key: string, mediaType: MediaType) {
  bindAll<HTMLButtonElement>(node, ".trial-requests", button => button.onclick = () => void hooks.navigate("requests"));
  bindAll<HTMLButtonElement>(node, ".trial-vote", button => button.onclick = () => {
    const card = button.closest<HTMLElement>(".trial-card")!;
    const rating = Number(pick<HTMLSelectElement>(card, ".trial-rating").value);
    if (!Number.isInteger(rating) || rating < 1 || rating > 5) {
      showError(new Error("Choose your rating from 1 to 5 before voting."));
      return;
    }
    const decision = button.dataset.decision as TrialDecision;
    void runControl(button, async () => {
      const controls = Array.from(card.querySelectorAll<HTMLButtonElement>(".trial-vote"));
      controls.forEach(control => control.disabled = true);
      try {
        await api(`/api/v1/trials/${card.dataset.trialId}/vote`, jsonRequest("POST", { decision, rating }));
        showToast(decision === "continue" ? "Rated and continued — the rest of the series is requested" : "Rated and stopped — shared requests continue");
        await discoverView(key, mediaType);
      } catch (error) {
        controls.forEach(control => control.disabled = false);
        throw error;
      }
    });
  });
}

function bindRecommendationCards(node: HTMLElement, recommendations: Recommendation[]) {
  for (const action of ["dismiss", "request", "rate"] as const) {
    bindAll<HTMLButtonElement>(node, `.rec-${action}`, button => {
      button.onclick = () => void recommendationAction(button, action);
    });
  }
  bindAll<HTMLButtonElement>(node, ".rec-preview", button => {
    button.onclick = () => {
      const recommendation = recommendations.find(value => value.id === Number(button.dataset.id))!;
      void recommendationPreview(button, recommendation);
    };
  });
  bindAll<HTMLSelectElement>(node, ".rec-monitor", select => select.onchange = () => {
    pick<HTMLElement>(select.closest(".request-scope")!, ".specific-seasons").hidden = select.value !== "specific";
  });
}

async function discoverPreferences(serverID: string, userID: string) {
  const query = new URLSearchParams({ server_id: serverID, user_id: userID });
  const preferences = await api<RecommendationPreferences>(`/api/v1/recommendations/preferences?${query}`);
  const dialog = openDialog(`<header class="preview-header"><h2>Recommendation preferences</h2><button class="command preferences-close" autofocus>Close</button></header>
    <form class="preferences-form"><label>Original languages (two-letter codes, comma separated)<input name="languages" value="${esc(preferences.languages.join(", "))}" placeholder="en, ja"></label>
    <label>Excluded genres (TMDB names, comma separated)<input name="genres" value="${esc(preferences.excluded_genres.join(", "))}" placeholder="Horror, Romance"></label>
    <label>Familiarity<select name="familiarity">${["balanced","familiar","explore"].map(value=>option(value,value,preferences.familiarity)).join("")}</select></label>
    <label>Diversity bonus (0–100%)<input name="diversity" type="number" min="0" max="100" value="${preferences.diversity}" required></label><p>Empty language and genre fields leave those choices unrestricted. Save, then Refresh Discover to apply.</p><button class="command" type="submit">Save preferences</button></form>`, ".preferences-close");
  pick<HTMLFormElement>(dialog, "form").onsubmit = event => {
    event.preventDefault();
    const form = new FormData(event.currentTarget as HTMLFormElement);
    const split = (name: string) => String(form.get(name)).split(",").map(value => value.trim()).filter(Boolean);
    void runControl(pick<HTMLButtonElement>(dialog, "[type=submit]"), async () => {
      await api(`/api/v1/recommendations/preferences?${query}`, jsonRequest("PUT", {
        languages: split("languages"), excluded_genres: split("genres"),
        familiarity: form.get("familiarity"), diversity: Number(form.get("diversity"))
      }));
      dialog.close();
      showToast("Preferences saved; refresh Discover to apply");
    });
  };
  dialog.showModal();
}

function feedbackHistoryRow(recommendation: Recommendation, ratingFor: Map<number, number>) {
  const rating = ratingFor.get(recommendation.tmdb_id);
  return `<article><strong>${esc(recommendation.title)}</strong><div class="row-actions"><label>Your rating<select class="history-rating">${[1,2,3,4,5].map(value=>option(String(value),String(value),String(rating ?? 5))).join("")}</select></label><button class="command compact history-action" data-action="rate" data-id="${recommendation.id}">Save rating</button>${!rating ? `<button class="command compact history-action" data-action="undo" data-id="${recommendation.id}">Undo dismissal</button>`:""}</div></article>`;
}

async function discoverHistory(serverID: string, userID: string, mediaType: MediaType) {
  const query = new URLSearchParams({ server_id: serverID, user_id: userID, media_type: mediaType });
  const history = await api<RecommendationHistoryResponse>(`/api/v1/recommendations/history?${query}`);
  const ratingFor = new Map(history.ratings.map(value => [value.tmdb_id, value.rating]));
  const dialog = openDialog(`<header class="preview-header"><h2>Rating &amp; dismissal history</h2><button class="command history-close" autofocus>Close</button></header><p>Up to 100 dismissed titles; ratings remain part of your taste profile.</p>
    <div class="feedback-history">${history.items.map(recommendation => feedbackHistoryRow(recommendation, ratingFor)).join("") || "No rating or dismissal history yet."}</div>`, ".history-close");
  bindAll<HTMLButtonElement>(dialog, ".history-action", button => button.onclick = () => void runControl(button, async () => {
    const action = button.dataset.action;
    const rating = Number(pick<HTMLSelectElement>(button.closest("article")!, "select").value);
    await api(`/api/v1/recommendations/${button.dataset.id}/actions`, jsonRequest("POST", {
      action_id: crypto.randomUUID(), action, ...(action === "rate" ? { rating } : {})
    }));
    dialog.close();
    showToast(action === "rate" ? "Rating updated" : "Dismissal undone");
    await discoverView();
  }));
  dialog.showModal();
}

function parsePreview(payload: unknown) {
  // Validate the new API boundary without adding a runtime dependency.
  if (!payload || typeof payload !== "object") throw new Error("Invalid preview response");
  const preview = payload as RecommendationPreview;
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
  return preview;
}

function previewMarkup(preview: RecommendationPreview) {
  return `<div class="preview-summary">${preview.poster_url ? `<img src="${esc(preview.poster_url)}" alt="${esc(preview.title)} poster">` : ""}
        <div><p class="preview-facts">${preview.media_type === "series" ? "Series" : "Movie"} · ${preview.year || "Year unknown"}${preview.runtime_minutes ? ` · ${preview.runtime_minutes} min${preview.media_type === "series" ? " / episode" : ""}` : ""}</p>
        <div class="audience-rating">${audienceRating(preview.vote_average, preview.vote_count)}</div>
        ${preview.genres.length ? `<p>${esc(preview.genres.join(" · "))}</p>` : ""}${preview.media_type === "series" && preview.seasons?.length ? `<p>Known seasons: ${esc(preview.seasons.join(", "))} (0 = specials)</p>` : ""}<p class="preview-overview">${esc(preview.overview || "No description available.")}</p>
        ${preview.people.length ? `<p class="preview-people"><strong>Cast &amp; filmmakers</strong><br>${esc(preview.people.join(", "))}</p>` : ""}</div></div>
        <section class="preview-trailers"><h3>Trailers &amp; teasers</h3>${preview.videos.length ? `<label>Video<select class="preview-video">${preview.videos.map((video, index) => `<option value="${index}">${esc(video.name)} (${video.official ? "Official " : ""}${esc(video.type)})</option>`).join("")}</select></label>
          <div class="preview-player"></div><div class="preview-video-actions"><button class="command preview-play">Play preview</button><a class="preview-external" target="_blank" rel="noopener noreferrer">Watch on YouTube</a></div><p>Video playback is provided by YouTube.</p>` : `<p>No trailer or teaser is available for this title.</p>`}</section>`;
}

function bindPreviewPlayer(previewContent: HTMLElement, preview: RecommendationPreview) {
  const videoSelect = pick<HTMLSelectElement>(previewContent, ".preview-video");
  const external = pick<HTMLAnchorElement>(previewContent, ".preview-external");
  const player = pick<HTMLElement>(previewContent, ".preview-player");
  const play = pick<HTMLButtonElement>(previewContent, ".preview-play");
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
}

async function recommendationPreview(button: HTMLButtonElement, recommendation: Recommendation) {
  const dialog = document.createElement("dialog");
  const controller = new AbortController();
  dialog.className = "preview-dialog";
  dialog.setAttribute("aria-labelledby", "preview-title");
  dialog.innerHTML = `<header class="preview-header"><h2 id="preview-title">${esc(recommendation.title)}</h2><button class="command compact preview-close" autofocus>Close</button></header>
    <div class="preview-content" aria-live="polite"><p>Loading preview…</p></div>`;
  document.body.append(dialog);
  pick<HTMLButtonElement>(dialog, ".preview-close").onclick = () => dialog.close();
  dialog.addEventListener("close", () => {
    controller.abort();
    dialog.remove();
    if (button.isConnected) button.focus();
  }, { once: true });
  dialog.showModal();
  const previewContent = pick<HTMLElement>(dialog, ".preview-content");
  const loadPreview = async () => {
    previewContent.innerHTML = `<p>Loading preview…</p>`;
    try {
      const payload = await api<unknown>(`/api/v1/recommendations/${recommendation.id}/preview`, { signal: controller.signal });
      if (controller.signal.aborted) return;
      const preview = parsePreview(payload);
      pick<HTMLElement>(button.closest(".recommendation-card")!, "[data-audience-rating]").innerHTML =audienceRating(preview.vote_average, preview.vote_count);
      previewContent.innerHTML = previewMarkup(preview);
      if (preview.videos.length) bindPreviewPlayer(previewContent, preview);
    } catch (error) {
      if (controller.signal.aborted) return;
      previewContent.innerHTML = `<p class="preview-overview">${esc(recommendation.overview || "No description available.")}</p><p class="preview-error" role="alert">${esc(error instanceof Error ? error.message : String(error))}</p><button class="command preview-retry">Retry preview</button>`;
      pick<HTMLButtonElement>(previewContent, ".preview-retry").onclick = () => void loadPreview();
    }
  };
  await loadPreview();
}

interface RecommendationScope {
  trial?: TrialScope;
  seasons?: number[];
  monitor_mode?: RequestMonitorMode;
}

// Returns null after reporting invalid season input.
function requestScope(card: HTMLElement): RecommendationScope | null {
  const scope = card.querySelector<HTMLSelectElement>(".rec-monitor")?.value;
  if (!scope) return {};
  if (trialScopes.includes(scope)) return { trial: scope as TrialScope };
  if (scope !== "specific") return { monitor_mode: scope as RequestMonitorMode };
  const seasonText = pick<HTMLInputElement>(card, ".rec-seasons").value;
  const seasons = seasonText.split(",").map(value => /^\d+$/.test(value.trim()) ? Number(value.trim()) : NaN);
  if (!seasonText.trim() || seasons.length > 100 || seasons.some(value => !Number.isInteger(value) || value < 0 || value > 999)) {
    showError(new Error("Enter season numbers between 0 and 999, separated by commas."));
    return null;
  }
  return { seasons };
}

async function recommendationAction(button: HTMLButtonElement, action: RecommendationAction) {
  const card = button.closest<HTMLElement>(".recommendation-card");
  if (!card) return;
  const controls = Array.from(card.querySelectorAll<HTMLButtonElement | HTMLSelectElement>("button, select"));
  const rating = action === "rate" ? Number(card.querySelector<HTMLSelectElement>(".rec-rating")?.value) : undefined;
  const scope = action === "request" ? requestScope(card) : {};
  if (!scope) return;
  controls.forEach(control => control.disabled = true);
  try {
    await api(`/api/v1/recommendations/${button.dataset.id}/actions`,
      jsonRequest("POST", { action_id: crypto.randomUUID(), action, ...(rating ? { rating } : {}), ...scope }));
    showToast(action === "request" ? "Added to Reelay" : action === "rate" ? `Rated ${rating} of 5` : "Recommendation dismissed");
    if (scope.trial) {
      await discoverView();
      return;
    }
    card.remove();
    const grid = document.querySelector<HTMLElement>(".recommendation-grid");
    if (grid && !grid.querySelector(".recommendation-card")) grid.innerHTML = `<div class="empty">No active recommendations</div>`;
  } catch (error) {
    controls.forEach(control => control.disabled = false);
    showError(error);
  }
}
