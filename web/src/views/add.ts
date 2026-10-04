import { api, esc } from "../api.ts";
import { session } from "../session.ts";
import type { ItemList, MetadataMatch, MetadataSearchResponse, QualityProfile } from "../types.ts";
import { bindAll, content, jsonRequest, pick, runControl, showToast } from "../ui.ts";

function resultCard(item: MetadataMatch, index: number, type: string, profiles: QualityProfile[]) {
  return `<article class="result">
      ${item.poster_url ? `<img src="${esc(item.poster_url)}" alt="">` : `<div class="poster-fallback">${esc(item.title.charAt(0))}</div>`}
      <div><h2>${esc(item.title)}</h2><p>${esc(item.year || "Year unknown")}</p><small>${esc(item.overview || "")}</small></div>
      <div class="add-controls"><select data-profile>${profiles.map(profile => `<option value="${profile.id}" ${profile.is_default ? "selected" : ""}>${esc(profile.name)}</option>`).join("")}</select>
      ${type === "series" ? `<select data-monitor><option value="future_only">Future only</option><option value="all">All episodes</option><option value="latest_season">Latest season</option></select>` : ""}
      <button class="command add-result" data-index="${index}">+ <span>Add</span></button></div></article>`;
}

function addRequestBody(type: string, result: MetadataMatch, parent: HTMLElement) {
  const profile_id = Number(pick<HTMLSelectElement>(parent, "[data-profile]").value);
  if (type === "series") {
    return { query: result.title, tvmaze_id: result.tvmaze_id, profile_id,
      monitor_mode: pick<HTMLSelectElement>(parent, "[data-monitor]").value };
  }
  return { query: result.title, tmdb_id: result.tmdb_id, year: result.year, profile_id };
}

export async function addView() {
  const profiles = (await api<ItemList<QualityProfile>>("/api/v1/profiles")).items ?? [];
  if (session.current !== "add") return;
  const node = content(`<div class="page-head"><div><h1>Add</h1><p>Find a movie or series</p></div></div>
    <form id="add-search" class="search-form"><div class="segmented"><label><input type="radio" name="type" value="series" checked><span>Series</span></label>
      <label><input type="radio" name="type" value="movie"><span>Movie</span></label></div>
      <input name="query" type="search" required placeholder="Title" autocomplete="off"><button class="command" type="submit">⌕ <span>Search</span></button></form>
    <div id="search-results" class="search-results"></div>`);
  pick<HTMLFormElement>(node, "#add-search").onsubmit = async event => {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    const formData = new FormData(form);
    const type = String(formData.get("type"));
    const query = String(formData.get("query"));
    await runControl(pick<HTMLButtonElement>(form, 'button[type="submit"]'), async () => {
      const payload = await api<MetadataSearchResponse>(`/api/v1/metadata/search?type=${type}&q=${encodeURIComponent(query)}`);
      if (session.current !== "add") return;
      const matches = payload.items ?? [];
      const results = pick<HTMLElement>(node, "#search-results");
      results.innerHTML = matches.map((match, index) => resultCard(match, index, type, profiles)).join("") || `<div class="empty">No matches</div>`;
      bindAll<HTMLButtonElement>(results, ".add-result", button => button.onclick = async () => {
        const result = matches[Number(button.dataset.index)];
        const body = addRequestBody(type, result, button.closest<HTMLElement>(".result")!);
        await runControl(button, async () => {
          await api(`/api/v1/${type === "series" ? "series" : "movies"}`, jsonRequest("POST", body));
          button.textContent = "Added";
          showToast(`${result.title} added`);
        }, undefined, true);
      });
    });
  };
}
