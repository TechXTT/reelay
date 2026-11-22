import { api, esc } from "../api.ts";
import { discoverUserKey, hooks, isMember, session } from "../session.ts";
import type { ItemList, JellyfinUser, MediaType, SavedForLaterResponse, TonightItem, TonightResponse } from "../types.ts";
import { bindAll, content, date, jsonRequest, option, pick, runControl, showError, showToast, userKey, userOptions } from "../ui.ts";

const runtimeChoices: [number, string][] = [
  [0, "Any length"], [30, "30 minutes"], [45, "45 minutes"], [60, "1 hour"], [90, "1½ hours"],
  [120, "2 hours"], [150, "2½ hours"], [180, "3 hours"]
];

function runtimeLabel(item: TonightItem) {
  if (!item.runtime_minutes) return "";
  return ` · ${item.runtime_minutes} min${item.media_type === "series" ? " / episode" : ""}`;
}

function playLink(item: TonightItem) {
  if (!item.jellyfin_url) return `<small class="tonight-note">Jellyfin link unavailable: no server URL is configured for this library.</small>`;
  return `<a class="command tonight-play" href="${esc(item.jellyfin_url)}" target="_blank" rel="noopener noreferrer">▶ Play in Jellyfin</a>`;
}

function poster(item: TonightItem) {
  if (item.poster_url) return `<img src="${esc(item.poster_url)}" alt="" loading="lazy">`;
  return `<div class="recommendation-poster">${esc(item.title.charAt(0))}</div>`;
}

function tonightCard(item: TonightItem) {
  return `<article class="tonight-card">${poster(item)}
    <div class="tonight-body"><div><h2>${esc(item.title)}</h2><small>${esc(item.media_type === "series" ? "Series" : "Movie")}${item.year ? ` · ${esc(item.year)}` : ""}${esc(runtimeLabel(item))}</small></div>
    ${item.genres?.length ? `<small class="recommendation-genres">${esc(item.genres.join(" · "))}</small>` : ""}
    ${item.completed ? `<span class="state state-available">Watched</span>` : ""}
    <div class="row-actions tonight-actions">${playLink(item)}
      ${item.saved_at ? `<button class="command compact tonight-unsave" data-item="${esc(item.item_id)}">Saved · Remove</button>` : `<button class="command compact tonight-save" data-item="${esc(item.item_id)}">Save for later</button>`}</div></div>
  </article>`;
}

function savedRow(item: TonightItem) {
  return `<li class="tonight-saved-row"><div><strong>${esc(item.title)}</strong><small>${esc(item.media_type === "series" ? "Series" : "Movie")}${item.year ? ` · ${esc(item.year)}` : ""}${esc(runtimeLabel(item))} · saved ${esc(date(item.saved_at))}</small>
    ${item.playable ? "" : `<small class="tonight-note">No longer in your Jellyfin libraries, so it cannot be played from here.</small>`}</div>
    <div class="row-actions">${item.playable ? playLink(item) : ""}<button class="command compact tonight-unsave" data-item="${esc(item.item_id)}">Remove</button></div></li>`;
}

function savedSection(saved: TonightItem[]) {
  if (!saved.length) return "";
  return `<section class="tonight-saved" aria-labelledby="tonight-saved-heading"><h2 id="tonight-saved-heading">Saved for later</h2>
    <ul>${saved.map(savedRow).join("")}</ul></section>`;
}

function tonightHead(user: JellyfinUser, users: JellyfinUser[], key: string) {
  return `<div class="page-head"><div><h1>Tonight</h1><p>Already in Jellyfin for ${esc(user.display_name)} · play something now</p></div>
    ${isMember() ? "" : `<div class="row-actions"><label class="request-user">Jellyfin user<select id="tonight-user">${userOptions(users, key)}</select></label></div>`}</div>
    <p class="tonight-intro">Everything here is ready to watch. Nothing is downloaded or requested from this page; to get a new title, use <button class="link-button" id="tonight-discover" type="button">Discover</button>.</p>`;
}

function tonightToolbar() {
  const type = session.tonightType;
  return `<div class="discover-toolbar tonight-toolbar">
    <div class="segmented" role="radiogroup" aria-label="Type"><label><input type="radio" name="tonight-type" value="" ${type === "" ? "checked" : ""}><span>All</span></label>
    <label><input type="radio" name="tonight-type" value="movie" ${type === "movie" ? "checked" : ""}><span>Movies</span></label>
    <label><input type="radio" name="tonight-type" value="series" ${type === "series" ? "checked" : ""}><span>Series</span></label></div>
    <label>Time available<select id="tonight-runtime">${runtimeChoices.map(([value, label]) => option(String(value), label, String(session.tonightMaxRuntime))).join("")}</select></label>
    <label class="tonight-switch"><input type="checkbox" id="tonight-unfinished" ${session.tonightUnfinished ? "checked" : ""}><span>Not finished yet</span></label></div>
    <p class="tonight-note">These choices last for this visit only.${session.tonightMaxRuntime ? " With a time limit, series use episode length and titles without a known length are hidden." : ""}</p>`;
}

function unsupportedNotice() {
  return `<div class="empty tonight-unsupported"><p><strong>Update the Reelay Jellyfin plugin to see your library here.</strong><br>
    Tonight lists only titles the plugin confirms you can open in Jellyfin, and this server has not reported that yet.</p></div>`;
}

export async function tonightView(selectedUser = session.discoverUser) {
  const users = (await api<ItemList<JellyfinUser>>("/api/v1/integrations/jellyfin/users")).items ?? [];
  if (session.current !== "tonight") return;
  if (!users.length) {
    content(`<div class="page-head"><div><h1>Tonight</h1><p>Play something already in Jellyfin</p></div></div>
      <div class="empty">Install and configure the Reelay Jellyfin plugin to view libraries by user.</div>`);
    return;
  }
  const user = users.find(value => userKey(value) === selectedUser) ?? users[0];
  const key = userKey(user);
  session.discoverUser = key;
  localStorage.setItem(discoverUserKey, key);
  const constraints = { type: session.tonightType, runtime: session.tonightMaxRuntime, unfinished: session.tonightUnfinished, offset: session.tonightOffset };
  const query = new URLSearchParams({ server_id: user.server_id, user_id: user.user_id, offset: String(constraints.offset) });
  if (constraints.type) query.set("media_type", constraints.type);
  if (constraints.runtime) query.set("max_runtime", String(constraints.runtime));
  if (constraints.unfinished) query.set("unfinished", "true");
  const scope = new URLSearchParams({ server_id: user.server_id, user_id: user.user_id });
  const [page, savedPage] = await Promise.all([
    api<TonightResponse>(`/api/v1/tonight?${query}`),
    api<SavedForLaterResponse>(`/api/v1/tonight/saved?${scope}`)
  ]);
  const stale = session.current !== "tonight" || session.discoverUser !== key || session.tonightType !== constraints.type ||
    session.tonightMaxRuntime !== constraints.runtime || session.tonightUnfinished !== constraints.unfinished || session.tonightOffset !== constraints.offset;
  if (stale) return;
  const titles = page.items ?? [];
  const saved = savedPage.items ?? [];
  const body = !page.supported ? unsupportedNotice() : `${tonightToolbar()}
    <p class="discover-freshness">Library checked ${page.library_synced_at ? esc(date(page.library_synced_at)) : "—"}</p>
    <div class="tonight-grid">${titles.length ? titles.map(tonightCard).join("") : `<div class="empty">Nothing matches these choices. Try a longer time or another type.</div>`}</div>
    <div class="row-actions tonight-pager"><button class="command tonight-previous" ${page.offset === 0 ? "disabled" : ""}>Previous</button><button class="command tonight-next" ${!page.has_more ? "disabled" : ""}>Next</button></div>`;
  const node = content(`${tonightHead(user, users, key)}${savedSection(saved)}${body}`);
  bindTonightView(node, user, key, page);
}

function bindTonightView(node: HTMLElement, user: JellyfinUser, key: string, page: TonightResponse) {
  const reload = () => void tonightView(key).catch(showError);
  const userSelect = node.querySelector<HTMLSelectElement>("#tonight-user");
  if (userSelect) userSelect.onchange = event => {
    session.tonightOffset = 0;
    void tonightView((event.currentTarget as HTMLSelectElement).value).catch(showError);
  };
  pick<HTMLButtonElement>(node, "#tonight-discover").onclick = () => void hooks.navigate("discover");
  bindAll<HTMLButtonElement>(node, ".tonight-save", button => button.onclick = () => void runControl(button, async () => {
    await api("/api/v1/tonight/saved", jsonRequest("POST", { server_id: user.server_id, user_id: user.user_id, item_id: button.dataset.item }));
    showToast("Saved for later");
    await tonightView(key);
  }));
  bindAll<HTMLButtonElement>(node, ".tonight-unsave", button => button.onclick = () => void runControl(button, async () => {
    const scope = new URLSearchParams({ server_id: user.server_id, user_id: user.user_id });
    await api(`/api/v1/tonight/saved/${encodeURIComponent(button.dataset.item ?? "")}?${scope}`, { method: "DELETE" });
    showToast("Removed from Save for later");
    await tonightView(key);
  }));
  if (!page.supported) return;
  bindAll<HTMLInputElement>(node, "input[name=tonight-type]", input => input.onchange = () => {
    session.tonightType = input.value as MediaType | "";
    session.tonightOffset = 0;
    reload();
  });
  pick<HTMLSelectElement>(node, "#tonight-runtime").onchange = event => {
    session.tonightMaxRuntime = Number((event.currentTarget as HTMLSelectElement).value);
    session.tonightOffset = 0;
    reload();
  };
  pick<HTMLInputElement>(node, "#tonight-unfinished").onchange = event => {
    session.tonightUnfinished = (event.currentTarget as HTMLInputElement).checked;
    session.tonightOffset = 0;
    reload();
  };
  pick<HTMLButtonElement>(node, ".tonight-previous").onclick = () => {
    session.tonightOffset = Math.max(0, session.tonightOffset - page.limit);
    reload();
  };
  pick<HTMLButtonElement>(node, ".tonight-next").onclick = () => {
    session.tonightOffset += page.limit;
    reload();
  };
}
