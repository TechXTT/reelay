import { api, esc } from "../api.ts";
import { discoverUserKey, hooks, requestsPageSize, session } from "../session.ts";
import type { DiagnosticsResponse, DiagnosticSubject, ItemList, JellyfinUser, MediaRequest, RequestListResponse, RequestMonitorMode } from "../types.ts";
import { bindAll, bindRequest, content, date, jsonRequest, openDialog, percent, pick, runControl, showError, showToast, state, userKey, userOptions } from "../ui.ts";
import { trialLabel } from "./discover.ts";

const retryableStates = ["wanted", "failed", "import_failed", "attention_needed", "searching", "cancelled"];

function monitorLabel(mode: RequestMonitorMode) {
  if (mode === "latest_season") return "Latest season";
  if (mode === "all") return "All episodes";
  return "Future episodes";
}

function requestsHead(user: JellyfinUser, users: JellyfinUser[], key: string, hasMore: boolean) {
  return `<div class="page-head requests-head"><div><h1>Recent requests</h1><p>Most recent requests for ${esc(user.display_name)} · follow each title until it is available in Jellyfin</p></div>
    <div class="row-actions"><label class="request-user">Jellyfin user<select id="requests-user">${userOptions(users, key)}</select></label>
    <button class="command" id="refresh-requests" aria-label="Refresh requests">↻ <span>Refresh</span></button></div></div>
    <div class="row-actions request-filters"><button class="command requests-attention" aria-pressed="${session.attentionOnly}">${session.attentionOnly ? "Show all requests" : "Attention needed"}</button><button class="command requests-previous" ${session.requestsOffset===0 ? "disabled" : ""}>Newer</button><button class="command requests-next" ${!hasMore ? "disabled" : ""}>Older</button></div>`;
}

function requestProgress(record: MediaRequest) {
  return `<div class="request-progress"><span>Progress</span>${record.media_type === "series" && record.total_episodes > 0 ? `<strong>${record.imported_episodes} of ${record.total_episodes} known episodes</strong>` : `<strong>${percent(record.progress).toFixed(0)}%</strong>`}
        ${record.progress > 0 && !record.available ? `<div class="progress"><i style="width:${percent(record.progress)}%"></i></div>` : ""}</div>`;
}

function requestTrialLine(record: MediaRequest) {
  if (!record.trial) return "";
  return `<small class="request-next">${esc(trialLabel(record.trial.scope))} · ${record.trial.watched_episodes} of ${record.trial.episode_limit || "?"} watched · ${esc(record.trial.state.replaceAll("_"," "))}</small>${record.trial.state==="awaiting_vote" ? `<button class="command trial-review">Rate and choose Continue / Stop in Discover</button>` : ""}`;
}

function requestControls(record: MediaRequest) {
  const retryable = (record.media_type === "series" || !record.available) && retryableStates.includes(record.state);
  return `<div class="row-actions request-controls">${record.jellyfin_url ? `<a class="preview-external" href="${esc(record.jellyfin_url)}" target="_blank" rel="noopener noreferrer">Open in Jellyfin</a>` : ""}
      ${retryable ? `<button class="command compact request-action" data-id="${record.id}" data-action="retry">Retry</button>` : ""}
      ${!record.cancelled_at ? `<button class="command compact request-action" data-id="${record.id}" data-action="cancel">Withdraw request</button>` : ""}<button class="command compact request-details" data-id="${record.id}">Diagnostics</button></div>`;
}

function requestRow(record: MediaRequest) {
  return `<article class="request-row">
      <div class="request-title"><strong>${esc(record.title)}</strong><small>${esc(record.media_type)}${record.year ? ` · ${esc(record.year)}` : ""}</small></div>
      <div class="request-status"><span>Status</span>${state(record.state)}${record.available ? `<span class="state state-available">Available in Jellyfin</span>` : ""}</div>
      ${requestProgress(record)}
      <div class="request-detail"><span>Requested</span><strong>${date(record.requested_at)}</strong></div>
      ${record.media_type === "series" && record.monitor_mode ? `<div class="request-detail"><span>Requested scope</span><strong>${esc(monitorLabel(record.monitor_mode))}</strong></div>` : ""}
      ${record.last_error ? `<p class="request-error">${esc(record.last_error)}</p>` : ""}
      ${record.next_search_at ? `<small class="request-next">Next search ${esc(date(record.next_search_at))}</small>` : ""}
      ${record.seasons?.length ? `<small class="request-next">Requested seasons: ${esc(record.seasons.join(", "))}</small>` : ""}
      ${requestTrialLine(record)}
      ${requestControls(record)}
    </article>`;
}

export async function requestsView(selectedUser = session.discoverUser) {
  const offset = session.requestsOffset;
  const attention = session.attentionOnly;
  const users = (await api<ItemList<JellyfinUser>>("/api/v1/integrations/jellyfin/users")).items ?? [];
  if (session.current !== "requests") return;
  if (!users.length) {
    content(`<div class="page-head"><div><h1>Requests</h1><p>Track recommendations through library availability</p></div></div>
      <div class="empty">Install and configure the Reelay Jellyfin plugin to view requests by user.</div>`);
    return;
  }
  const user = users.find(value => userKey(value) === selectedUser) ?? users[0];
  const key = userKey(user);
  session.discoverUser = key;
  localStorage.setItem(discoverUserKey, key);
  const query = `server_id=${encodeURIComponent(user.server_id)}&user_id=${encodeURIComponent(user.user_id)}&offset=${offset}&attention=${attention}`;
  const requestPage = await api<RequestListResponse>(`/api/v1/requests?${query}`);
  const records = requestPage.items;
  if (session.current !== "requests" || session.discoverUser !== key || session.requestsOffset !== offset || session.attentionOnly !== attention) return;
  const node = content(`${requestsHead(user, users, key, requestPage.has_more)}
    <div class="request-list">${records.length ? records.map(requestRow).join("") : `<div class="empty">No requests for ${esc(user.display_name)} yet. Request a recommendation from Discover to track it here.</div>`}</div>`);
  bindRequestsView(node, key);
}

function bindRequestsView(node: HTMLElement, key: string) {
  const reload = () => void requestsView(key).catch(showError);
  pick<HTMLSelectElement>(node, "#requests-user").onchange = event => {
    session.requestsOffset = 0;
    void requestsView((event.currentTarget as HTMLSelectElement).value).catch(showError);
  };
  const refresh = pick<HTMLButtonElement>(node, "#refresh-requests");
  refresh.onclick = () => void runControl(refresh, () => requestsView(key));
  bindAll<HTMLButtonElement>(node, ".trial-review", button => button.onclick = () => void hooks.navigate("discover"));
  bindAll<HTMLButtonElement>(node, ".request-action", button => {
    const action = button.dataset.action;
    bindRequest(button, `/api/v1/requests/${button.dataset.id}/actions`, jsonRequest("POST", { action }),
      action === "cancel" ? "Request withdrawn; shared downloads continue" : "Retry scheduled", () => requestsView(key));
  });
  bindAll<HTMLButtonElement>(node, ".request-details", button => button.onclick = () =>
    void requestDiagnostics(Number(button.dataset.id)).catch(showError));
  pick<HTMLButtonElement>(node, ".requests-attention").onclick = () => {
    session.attentionOnly = !session.attentionOnly;
    session.requestsOffset = 0;
    reload();
  };
  pick<HTMLButtonElement>(node, ".requests-previous").onclick = () => {
    session.requestsOffset = Math.max(0, session.requestsOffset - requestsPageSize);
    reload();
  };
  pick<HTMLButtonElement>(node, ".requests-next").onclick = () => {
    session.requestsOffset += requestsPageSize;
    reload();
  };
}

function diagnosticSubject(subject: DiagnosticSubject) {
  return `<section><h3>${esc(subject.title || `${subject.type} #${subject.id}`)}</h3>${state(subject.state)}${subject.error ? `<p class="preview-error">${esc(subject.error)}</p>` : ""}
    <p>Search attempts: ${esc(subject.search_attempts ?? 0)} · Last search: ${subject.last_search_at ? esc(date(subject.last_search_at)) : "Not in recent history"}${subject.next_search_at ? ` · Next retry: ${esc(date(subject.next_search_at))}` : ""}</p>
    <ol>${(subject.history ?? []).map(transition => `<li>${date(transition.at)}: ${esc(transition.reason)} ${esc(transition.detail || "")}</li>`).join("")}</ol>
    <div class="diagnostic-candidates">${subject.candidates.map(candidate => `<article><strong>${esc(candidate.release.raw_title)}</strong><p>${candidate.evaluation.accepted ? `Accepted · score ${candidate.evaluation.score}` : "Rejected"}: ${esc(candidate.evaluation.reason)}</p>${candidate.evaluation.accepted ? `<button class="command compact candidate-grab" data-subject="${subject.id}" data-type="${subject.type}" data-release="${candidate.release.id}">Select release</button>` : ""}</article>`).join("") || "No candidates persisted yet."}</div></section>`;
}

async function requestDiagnostics(id: number) {
  const payload = await api<DiagnosticsResponse>(`/api/v1/requests/${id}/diagnostics`);
  const dialog = openDialog(`<header class="preview-header"><h2>Request diagnostics</h2><button class="command diagnostics-close" autofocus>Close</button></header><p>Latest persisted history and evaluations. Up to 50 subjects and 100 candidates per subject.</p>
    ${payload.subjects.map(diagnosticSubject).join("") || `<p>No active episode diagnostics are available.</p>`}`, ".diagnostics-close");
  bindAll<HTMLButtonElement>(dialog, ".candidate-grab", button => button.onclick = () => void runControl(button, async () => {
    await api(`/api/v1/requests/${id}/grab`, jsonRequest("POST", {
      subject_type: button.dataset.type, subject_id: Number(button.dataset.subject), release_id: Number(button.dataset.release)
    }));
    dialog.close();
    showToast("Release selected");
    await requestsView();
  }));
  dialog.showModal();
}
