import { api, esc } from "../api.ts";
import { session, subtitleBatchLimit, subtitleLanguageKey, subtitlesPageSize } from "../session.ts";
import type { SubtitleCount, SubtitleCoverageRow, SubtitleJob, SubtitleJobStateName, SubtitleOverview, SubtitlePage } from "../types.ts";
import { bindAll, content, date, jsonRequest, openDialog, pick, runControl, showError, showToast, state, table } from "../ui.ts";

const languagePattern = /^[a-z]{3}$/;
const retryable: SubtitleJobStateName[] = ["failed", "no_match", "needs_review", "stale", "cancelled", "covered"];
const cancellable: SubtitleJobStateName[] = ["queued", "running", "waiting", "needs_review", "failed", "no_match"];
let viewGeneration = 0;

// Plain-language explanations of the outcome codes the plugin and store record.
const outcomeText: Record<string, string> = {
  downloaded: "Downloaded; waiting for Jellyfin to list the track",
  confirmed: "Jellyfin lists the new track",
  covered: "Subtitles were already present",
  subtitles_present: "Subtitles were already present",
  file_replaced: "The file was replaced; it is inspected again",
  source_removed: "The file is no longer in Jellyfin",
  stale: "The file changed or is not probed yet",
  no_match: "No matching subtitles returned by the provider",
  weak_match: "Only weak matches; choose one or cancel",
  no_provider: "No enabled subtitle provider for this item",
  rate_limited: "Provider limit reached; downloads are paused",
  failed: "Download failed",
  not_visible: "Waiting for Jellyfin to list the track",
  track_not_listed: "Jellyfin never listed the downloaded track",
  lease_expired: "Interrupted; checking whether the download finished before trying again",
  unsupported_plugin: "The Jellyfin plugin no longer reports subtitle coverage",
  cancelled: "Cancelled"
};

function episodeCode(season: number | null, episode: number | null) {
  return `S${String(season ?? 0).padStart(2, "0")}E${String(episode ?? 0).padStart(2, "0")}`;
}

function itemLabel(row: Pick<SubtitleCoverageRow, "media_type" | "title" | "series_title" | "season" | "episode" | "edition">) {
  const name = row.media_type === "episode" ? `${row.series_title} ${episodeCode(row.season, row.episode)} · ${row.title}` : row.title;
  return row.edition ? `${name} [${row.edition}]` : name;
}

function count(counts: SubtitleCount[], status: string, mediaType?: string) {
  return counts.filter(entry => entry.status === status && (!mediaType || entry.media_type === mediaType)).reduce((sum, entry) => sum + entry.count, 0);
}

function summary(overview: SubtitleOverview) {
  const coverageRows = (["movie", "episode"] as const).map(mediaType => [mediaType === "movie" ? "Movies" : "Episodes",
    String(count(overview.coverage, "present", mediaType)), String(count(overview.coverage, "missing", mediaType)), String(count(overview.coverage, "unknown", mediaType))]);
  const jobStates: SubtitleJobStateName[] = ["queued", "running", "waiting", "confirmed", "no_match", "needs_review", "failed", "covered", "stale", "cancelled"];
  const jobRows = jobStates.filter(name => count(overview.jobs, name) > 0).map(name => [state(name), String(count(overview.jobs, name))]);
  return `<div class="subtitle-summary"><section><h2>Server coverage</h2>${table(["", "With subtitles", "Without", "Not probed"], coverageRows)}</section>
    <section><h2>Downloads</h2>${table(["State", "Count"], jobRows)}</section></div>`;
}

function notices(overview: SubtitleOverview) {
  const notes: string[] = [];
  if (!overview.audit) notes.push("No coverage report yet. The Reelay plugin reports coverage during its sync, or on its next check after Audit now. Plugins older than this Reelay never report, so every item stays unknown.");
  if (overview.audit && !overview.audit.providers.length) notes.push("Jellyfin has no subtitle provider installed. Audits work, but downloads will fail until one is added, for example the OpenSubtitles plugin.");
  if (overview.audit_error) notes.push(`The last audit could not finish: ${overview.audit_error}`);
  if (overview.audit_requested) notes.push(overview.audit_library ? `An audit of the ${overview.audit_library} library is waiting for the plugin's next check.` : "A full audit is waiting for the plugin's next check.");
  if (overview.audit?.audit_library) notes.push(`The last audit covered only the ${overview.audit.audit_library} library. ${overview.audit.last_full_audit_at ? `The last full audit was ${date(overview.audit.last_full_audit_at)}.` : "No full audit has completed yet, so new media is not followed."}`);
  if (overview.paused_until) notes.push(`A provider limit was reached. Downloads resume after ${date(overview.paused_until)}.`);
  for (const library of overview.audit?.libraries ?? []) {
    if (library.download_languages.length) notes.push(`${library.name} uses ${library.download_languages.join(", ")} for Jellyfin subtitle downloads. Reelay skips these languages while Jellyfin’s Download Missing Subtitles task is running or has a scheduled trigger.`);
  }
  return notes.map(note => `<p class="upgrade-note">${esc(note)}</p>`).join("");
}

function jobBadges(row: SubtitleCoverageRow) {
  return row.jobs.map(job => `${state(job.state)}<small>${esc(job.language)}</small>`).join(" ");
}

function coverageRow(row: SubtitleCoverageRow) {
  const selectable = row.status === "missing";
  return `<article class="upgrade-row subtitle-row">
      <div class="collection-title">${selectable ? `<label><input type="checkbox" class="subtitle-select" value="${esc(row.media_source_id)}" 
        aria-label="Select ${esc(itemLabel(row))}"> <strong>${esc(itemLabel(row))}</strong></label>` : `<strong>${esc(itemLabel(row))}</strong>`}
        <small>${esc(row.library || row.media_type)}${row.year ? ` · ${row.year}` : ""}</small></div>
      <div class="collection-field"><span>Subtitles</span><strong>${row.subtitle_languages.length ? esc(row.subtitle_languages.join(", ")) : row.status === "unknown" ? "Not probed" : row.status === "present" ? "Language unknown" : "None"}</strong></div>
      <div class="collection-field"><span>Checked</span><strong>${esc(date(row.checked_at))}</strong></div>
      <div class="collection-field"><span>Downloads</span>${row.jobs.length ? jobBadges(row) : "<strong>None</strong>"}</div>
      <div class="row-actions">${row.jellyfin_url ? `<a class="command compact" href="${esc(row.jellyfin_url)}" target="_blank" rel="noopener">Open in Jellyfin</a>` : ""}</div></article>`;
}

function jobRow(job: SubtitleJob) {
  const title = job.title ? itemLabel({ ...job, media_type: job.media_type || "movie" }) : `Removed source ${job.media_source_id}`;
  const actions = [
    job.state === "needs_review" ? `<button class="command compact subtitle-review" data-id="${job.id}">Choose</button>` : "",
    retryable.includes(job.state) ? `<button class="command compact subtitle-action" data-id="${job.id}" data-action="retry">Retry</button>` : "",
    cancellable.includes(job.state) ? `<button class="command compact danger subtitle-action" data-id="${job.id}" data-action="cancel">Cancel</button>` : ""
  ].join("");
  const next = job.next_attempt_at ? `Next ${esc(date(job.next_attempt_at))}` : "Nothing scheduled";
  return `<article class="upgrade-row subtitle-row">
      <div class="collection-title"><strong>${esc(title)}</strong><small>${esc(job.language)} · ${job.origin === "import" ? "new media" : "backfill"}${job.provider ? ` · ${esc(job.provider)}` : ""}</small></div>
      <div class="collection-field"><span>State</span>${state(job.state)}<small>${esc(outcomeText[job.outcome] ?? job.outcome)}</small></div>
      <div class="collection-field"><span>Attempts</span><strong>${job.attempts}</strong><small>${next}</small></div>
      <div class="collection-field"><span>Updated</span><strong>${esc(date(job.updated_at))}</strong></div>
      <div class="row-actions">${actions}</div>
      ${job.detail ? `<details class="subtitle-detail"><summary>Detail</summary><code>${esc(job.detail)}</code></details>` : ""}</article>`;
}

function chooseCandidate(job: SubtitleJob, reload: () => Promise<void>) {
  const rows = job.candidates.map(candidate => [
    `${esc(candidate.name || candidate.id)}<small> ${esc(candidate.format)}</small>`,
    esc(candidate.provider),
    [candidate.hash_match ? "matches file" : "not matched to file", candidate.forced ? "forced only" : "", candidate.hearing_impaired ? "SDH" : "",
      candidate.machine_translated ? "machine translated" : ""].filter(Boolean).map(esc).join(", "),
    String(candidate.downloads),
    `<button class="command compact subtitle-choose" data-subtitle="${esc(candidate.id)}">Download</button>`
  ]);
  const dialog = openDialog(`<header><h2>Choose subtitles</h2><p>${esc(itemLabel({ ...job, media_type: job.media_type || "movie" }))} · ${esc(job.language)}</p>
    <button class="command compact" id="subtitle-dialog-close">Close</button></header>
    <p class="upgrade-note">Check the release name, file match and track type before choosing a subtitle.</p>
    ${table(["Release", "Provider", "Match", "Downloads", ""], rows)}`, "#subtitle-dialog-close");
  bindAll<HTMLButtonElement>(dialog, ".subtitle-choose", button => button.onclick = () => void runControl(button, async () => {
    await api(`/api/v1/subtitles/jobs/${job.id}/actions`, jsonRequest("POST", { action: "choose", subtitle_id: button.dataset.subtitle }));
    dialog.close();
    showToast("Download queued");
    await reload();
  }));
  dialog.showModal();
}

function tabs(overview: SubtitleOverview) {
  const jobsTotal = overview.jobs.reduce((sum, entry) => sum + entry.count, 0);
  const options = [
    ["missing", `Without subtitles (${count(overview.coverage, "missing")})`],
    ["unknown", `Not probed (${count(overview.coverage, "unknown")})`],
    ["present", `With subtitles (${count(overview.coverage, "present")})`],
    ["jobs", `Downloads (${jobsTotal})`]
  ];
  return `<div class="segmented subtitle-tabs" role="radiogroup" aria-label="Subtitle list">${options.map(([value, label]) => `<label>
    <input type="radio" name="subtitle-tab" value="${value}" ${session.subtitleTab === value ? "checked" : ""}><span>${esc(label)}</span></label>`).join("")}</div>`;
}

function controls(overview: SubtitleOverview) {
  const follow = overview.follow_language;
  return `<div class="subtitle-controls">
    ${overview.servers.length > 1 ? `<label class="collection-field"><span>Jellyfin server</span><select id="subtitle-server">${overview.servers.map(id =>
      `<option value="${esc(id)}" ${id === overview.server_id ? "selected" : ""}>${esc(id)}</option>`).join("")}</select></label>` : ""}
    <label class="collection-field"><span>Language (ISO 639-2)</span><input id="subtitle-language" value="${esc(session.subtitleLanguage)}" maxlength="3"
      autocapitalize="none" spellcheck="false" pattern="[a-z]{3}" placeholder="eng"></label>
    <button class="command" id="subtitle-batch" disabled ${session.subtitleTab === "missing" ? "" : "hidden"}>Download subtitles</button>
    <button class="command" id="subtitle-audit" ${overview.audit_requested ? "disabled" : ""}>${overview.audit_requested ? "Audit requested" : session.subtitleLibrary ? `Audit ${esc(session.subtitleLibrary)} now` : "Audit all libraries now"}</button>
    <button class="command" id="subtitle-follow">${follow ? `Stop following new media (${esc(follow)})` : "Follow new media in this language"}</button></div>`;
}

async function listBody(serverID: string, offset: number) {
  if (session.subtitleTab === "jobs") {
    const page = await api<SubtitlePage<SubtitleJob>>(`/api/v1/subtitles/jobs?server_id=${encodeURIComponent(serverID)}&limit=${subtitlesPageSize}&offset=${offset}`);
    return { total: page.total, rows: [] as SubtitleCoverageRow[], hasMore: page.has_more, jobs: page.items, html: page.items.length ? page.items.map(jobRow).join("") : `<div class="empty">No subtitle downloads yet.</div>` };
  }
  const mediaType = session.subtitleMediaType ? `&media_type=${session.subtitleMediaType}` : "";
  const page = await api<SubtitlePage<SubtitleCoverageRow>>(`/api/v1/subtitles/coverage?server_id=${encodeURIComponent(serverID)}&status=${session.subtitleTab}&library=${encodeURIComponent(session.subtitleLibrary)}${mediaType}&limit=${subtitlesPageSize}&offset=${offset}`);
  return { total: page.total, rows: page.items, hasMore: page.has_more, jobs: [] as SubtitleJob[], html: page.items.length ? page.items.map(coverageRow).join("") : `<div class="empty">Nothing in this list.</div>` };
}

export async function subtitlesView() {
  const generation = ++viewGeneration;
  const current = () => session.current === "subtitles" && generation === viewGeneration;
  const overview = await api<SubtitleOverview>(`/api/v1/subtitles${session.subtitleServer ? `?server_id=${encodeURIComponent(session.subtitleServer)}` : ""}`);
  if (!current()) return;
  if (!overview.server_id) {
    content(`<div class="page-head"><div><h1>Subtitles</h1><p>No Jellyfin server has synchronized with Reelay yet.</p></div></div>`);
    return;
  }
  session.subtitleServer = overview.server_id;
  const offset = session.subtitleOffset;
  const { total, rows, hasMore, jobs, html } = await listBody(overview.server_id, offset);
  if (!current()) return;
  const audit = overview.audit
    ? `Coverage reported ${date(overview.audit.completed_at)} · providers: ${overview.audit.providers.length ? overview.audit.providers.join(", ") : "none"}`
    : "Coverage has not been reported yet";
  const library = session.subtitleLibrary;
  const libraryNames = new Set((overview.audit?.libraries ?? []).map(entry => entry.name));
  if (library) libraryNames.add(library);
  const mediaType = session.subtitleMediaType;
  const node = content(`<div class="page-head"><div><h1>Subtitles</h1><p>${esc(audit)}. Items with any subtitle track, including forced-only or unknown-language tracks, count as covered.</p></div></div>
    ${notices(overview)}${summary(overview)}${controls(overview)}${tabs(overview)}
    ${session.subtitleTab === "jobs" ? "" : `<div class="row-actions request-filters"><select id="subtitle-library" aria-label="Library">
      <option value="">All libraries</option>${[...libraryNames].map(name => `<option value="${esc(name)}" ${name === library ? "selected" : ""}>${esc(name)}</option>`).join("")}
      </select><select id="subtitle-media-type" aria-label="Media type">
      <option value="">Movies and episodes</option><option value="movie" ${session.subtitleMediaType === "movie" ? "selected" : ""}>Movies</option>
      <option value="episode" ${session.subtitleMediaType === "episode" ? "selected" : ""}>Episodes</option></select><span>${total} matching sources</span></div>`}
    ${session.subtitleTab === "jobs" ? '<div class="row-actions request-filters"><button class="command" id="subtitle-refresh">Refresh downloads</button></div>' : ""}
    <div class="upgrade-list">${html}</div>
    ${offset > 0 || hasMore ? `<div class="row-actions request-filters"><button class="command subtitle-previous" ${offset === 0 ? "disabled" : ""}>Previous</button>
      <button class="command subtitle-next" ${hasMore ? "" : "disabled"}>Next</button></div>` : ""}`);
  const reload = async () => { if (current()) await subtitlesView(); };
  const refreshButton = node.querySelector<HTMLButtonElement>("#subtitle-refresh");
  if (refreshButton) refreshButton.onclick = () => void runControl(refreshButton, reload);
  const selected = new Set<string>();
  const languageInput = pick<HTMLInputElement>(node, "#subtitle-language");
  const readLanguage = () => {
    const language = languageInput.value.trim().toLowerCase();
    if (!languagePattern.test(language)) throw new Error("Enter a three-letter ISO 639-2 language code, such as eng or fre.");
    session.subtitleLanguage = language;
    try { localStorage.setItem(subtitleLanguageKey, language); } catch { /* the language is a convenience only */ }
    return language;
  };
  languageInput.onchange = () => {
    try {
      readLanguage();
      updateSelection();
    } catch (error) {
      showError(error);
    }
  };
  const server = node.querySelector<HTMLSelectElement>("#subtitle-server");
  if (server) server.onchange = () => {
    session.subtitleServer = server.value;
    session.subtitleOffset = 0;
    session.subtitleLibrary = "";
    void subtitlesView().catch(showError);
  };
  bindAll<HTMLInputElement>(node, "input[name=subtitle-tab]", input => input.onchange = () => {
    session.subtitleTab = input.value as typeof session.subtitleTab;
    session.subtitleOffset = 0;
    void subtitlesView().catch(showError);
  });
  const librarySelect = node.querySelector<HTMLSelectElement>("#subtitle-library");
  if (librarySelect) librarySelect.onchange = () => {
    session.subtitleLibrary = librarySelect.value;
    session.subtitleOffset = 0;
    void subtitlesView().catch(showError);
  };
  const mediaTypeSelect = node.querySelector<HTMLSelectElement>("#subtitle-media-type");
  if (mediaTypeSelect) mediaTypeSelect.onchange = () => {
    session.subtitleMediaType = mediaTypeSelect.value as typeof session.subtitleMediaType;
    session.subtitleOffset = 0;
    void subtitlesView().catch(showError);
  };
  const batch = pick<HTMLButtonElement>(node, "#subtitle-batch");
  const checkboxes = [...node.querySelectorAll<HTMLInputElement>(".subtitle-select")];
  let previewIDs: string[] = [];
  const updateSelection = () => {
    const eligible = new Set(rows.filter(row => row.status === "missing" && !row.jobs.some(job =>
      job.language === session.subtitleLanguage && job.blocks_batch)).map(row => row.media_source_id));
    for (const checkbox of checkboxes) {
      checkbox.disabled = !eligible.has(checkbox.value);
      if (checkbox.disabled) selected.delete(checkbox.value);
      checkbox.checked = selected.has(checkbox.value);
    }
    previewIDs = selected.size ? [...selected] : [...eligible].slice(0, subtitleBatchLimit);
    batch.disabled = !overview.audit || !previewIDs.length;
    batch.textContent = selected.size ? `Download for ${selected.size} selected` :
      previewIDs.length ? `Download for first ${previewIDs.length} eligible on this page` : "No eligible downloads on this page";
  };
  updateSelection();
  for (const input of checkboxes) input.onchange = () => {
    if (input.checked && selected.size >= subtitleBatchLimit) {
      input.checked = false;
      showError(new Error(`A batch takes at most ${subtitleBatchLimit} items.`));
      return;
    }
    if (input.checked) selected.add(input.value);
    else selected.delete(input.value);
    updateSelection();
  };
  batch.onclick = () => void runControl(batch, async () => {
    if (!current()) return;
    const language = readLanguage();
    const ids = previewIDs;
    if (!ids.length) return;
    const result = await api<{ queued: number }>("/api/v1/subtitles/batches", jsonRequest("POST", {
      server_id: overview.server_id, library, media_type: mediaType, language, media_source_ids: ids, limit: ids.length
    }));
    showToast(result.queued ? `${result.queued} ${language} ${result.queued === 1 ? "download" : "downloads"} queued` : "Nothing new to queue");
    await reload();
  });
  const auditButton = pick<HTMLButtonElement>(node, "#subtitle-audit");
  auditButton.onclick = () => void runControl(auditButton, async () => {
    if (!current()) return;
    await api("/api/v1/subtitles/audit", jsonRequest("POST", { server_id: overview.server_id, library }));
    showToast("Audit requested for the plugin’s next check");
    await reload();
  });
  const followButton = pick<HTMLButtonElement>(node, "#subtitle-follow");
  followButton.onclick = () => void runControl(followButton, async () => {
    if (!current()) return;
    const language = overview.follow_language ? "" : readLanguage();
    await api("/api/v1/subtitles/follow", jsonRequest("PUT", { server_id: overview.server_id, language }));
    showToast(language ? `New media without subtitles will get ${language}` : "Stopped following new media");
    await reload();
  });
  bindAll<HTMLButtonElement>(node, ".subtitle-action", button => button.onclick = () => void runControl(button, async () => {
    if (!current()) return;
    await api(`/api/v1/subtitles/jobs/${button.dataset.id}/actions`, jsonRequest("POST", { action: button.dataset.action }));
    showToast(button.dataset.action === "retry" ? "Download queued again" : "Download cancelled");
    await reload();
  }));
  bindAll<HTMLButtonElement>(node, ".subtitle-review", button => button.onclick = () => {
    const job = jobs.find(item => String(item.id) === button.dataset.id);
    if (current() && job) chooseCandidate(job, reload);
  });
  if (offset > 0 || hasMore) {
    pick<HTMLButtonElement>(node, ".subtitle-previous").onclick = () => {
      session.subtitleOffset = Math.max(0, offset - subtitlesPageSize);
      void subtitlesView().catch(showError);
    };
    pick<HTMLButtonElement>(node, ".subtitle-next").onclick = () => {
      session.subtitleOffset = offset + subtitlesPageSize;
      void subtitlesView().catch(showError);
    };
  }
}

