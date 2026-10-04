import { api, esc } from "../api.ts";
import { hooks, session } from "../session.ts";
import type { Grab, ItemList, Movie, QualityProfile, QueueResponse } from "../types.ts";
import { bindAll, bindRequest, content, isEditing, jsonRequest, pick, profileOptions, runControl, showToast, state } from "../ui.ts";
import { cancelGrab, deleteCollection } from "./collection.ts";

function movieRow(movie: Movie, profiles: QualityProfile[], hasActiveGrab: boolean) {
  return `<article class="collection-row">
      <div class="collection-title"><strong>${esc(movie.title)}</strong><small>${esc(movie.year)}</small></div>
      <div class="collection-field"><span>State</span>${state(movie.state)}</div>
      <div class="collection-field"><span>Quality</span><strong>${esc(movie.imported_quality || "Not imported")}</strong></div>
      <label class="collection-field">Profile<select class="table-select movie-profile" data-id="${movie.id}">${profileOptions(profiles, movie.quality_profile_id)}</select></label>
      <div class="row-actions"><button class="command compact movie-search" data-id="${movie.id}">Search</button>
      ${hasActiveGrab ? `<button class="command compact danger movie-cancel" data-id="${movie.id}">Cancel</button>` : ""}
      <button class="command compact danger movie-delete" data-id="${movie.id}">Delete</button></div></article>`;
}

export async function moviesView() {
  const [payload, profilePage, queue] = await Promise.all([
    api<ItemList<Movie>>("/api/v1/movies"), api<ItemList<QualityProfile>>("/api/v1/profiles"), api<QueueResponse>("/api/v1/queue")
  ]);
  if (session.current !== "movies" || (session.refreshing && isEditing())) return;
  const movies = payload.items ?? [];
  const profiles = profilePage.items ?? [];
  const activeByMovie = new Map<number, Grab>((queue.items ?? [])
    .filter(grab => grab.subject_type === "movie").map(grab => [grab.subject_id, grab]));
  const node = content(`<div class="page-head"><div><h1>Movies</h1><p>${movies.length} tracked</p></div>
    <button class="command" data-view="add">+ <span>Add movie</span></button></div>
    <div class="collection-list">${movies.length ? movies.map(movie => movieRow(movie, profiles, activeByMovie.has(movie.id))).join("") : `<div class="empty">No tracked movies</div>`}</div>`);
  const movieFor = (button: HTMLElement) => movies.find(movie => movie.id === Number(button.dataset.id));
  pick<HTMLElement>(node, "[data-view=add]").onclick = () => hooks.navigate("add");
  bindAll<HTMLButtonElement>(node, ".movie-search", button =>
    bindRequest(button, `/api/v1/movies/${button.dataset.id}/search`, { method: "POST" }, "Movie search started"));
  bindAll<HTMLSelectElement>(node, ".movie-profile", select => select.onchange = async () => {
    const movie = movieFor(select);
    if (!movie) return;
    await runControl(select, async () => {
      const profileID = Number(select.value);
      await api(`/api/v1/movies/${select.dataset.id}`, jsonRequest("PATCH", { profile_id: profileID }));
      movie.quality_profile_id = profileID;
      showToast("Movie profile updated");
    }, () => { select.value = String(movie.quality_profile_id); });
  });
  bindAll<HTMLButtonElement>(node, ".movie-cancel", button => button.onclick = () => {
    const grab = activeByMovie.get(Number(button.dataset.id));
    const movie = movieFor(button);
    if (grab && movie) void cancelGrab(grab, movie.title);
  });
  bindAll<HTMLButtonElement>(node, ".movie-delete", button => button.onclick = () => {
    const movie = movieFor(button);
    if (movie) void deleteCollection("movies", movie.id, movie.title,
      activeByMovie.has(movie.id), Boolean(movie.imported_path));
  });
}
