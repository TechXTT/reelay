import { api, esc } from "../api.ts";
import { session, upgradesPageSize } from "../session.ts";
import type { UpgradeItem, UpgradeListResponse } from "../types.ts";
import { bindAll, bindRequest, content, date, pick, showError, state } from "../ui.ts";

function upgradeRow(item: UpgradeItem, canStart: boolean) {
  const action = item.upgrading ? "" : `<button class="command compact upgrade-search" data-subject="${esc(item.subject_type)}"
    data-id="${item.id}" aria-label="Search now for a better release of ${esc(item.title)}" ${canStart ? "" : "disabled"}>Search now</button>`;
  return `<article class="upgrade-row">
      <div class="collection-title"><strong>${esc(item.title)}</strong><small>${esc(item.subject_type)} · ${esc(item.profile)}</small></div>
      <div class="collection-field"><span>Quality</span><strong>${esc(item.quality)} → ${esc(item.cutoff)}</strong></div>
      <div class="collection-field"><span>Last checked</span><strong>${item.upgrade_checked_at ? esc(date(item.upgrade_checked_at)) : "Never"}</strong></div>
      <div class="collection-field"><span>Status</span>${state(item.upgrading ? "upgrading" : "waiting")}${item.upgrading ? `<small>${esc(item.state.replaceAll("_", " "))}</small>` : ""}</div>
      <div class="row-actions">${action}</div></article>`;
}

function upgradesSummary(page: UpgradeListResponse) {
  if (!page.enabled) return "Automatic upgrades are off. Set schedules.upgrade_interval to turn them on.";
  return `${page.inflight} of ${page.max_upgrade_grabs} upgrade downloads in use · ${page.total} below cutoff`;
}

function upgradesEmpty(page: UpgradeListResponse) {
  return page.enabled
    ? "Nothing is below its profile's upgrade cutoff."
    : "Automatic upgrades are off, and nothing is below its profile's upgrade cutoff.";
}

export async function upgradesView() {
  const offset = session.upgradesOffset;
  const page = await api<UpgradeListResponse>(`/api/v1/upgrades?limit=${upgradesPageSize}&offset=${offset}`);
  if (session.current !== "upgrades" || session.upgradesOffset !== offset) return;
  const items = page.items ?? [];
  if (!items.length && offset > 0) {
    session.upgradesOffset = 0;
    await upgradesView();
    return;
  }
  const canStart = page.enabled && page.inflight < page.max_upgrade_grabs;
  const node = content(`<div class="page-head"><div><h1>Upgrades</h1><p>${esc(upgradesSummary(page))}</p></div></div>
    ${page.truncated ? `<p class="upgrade-note">Only the first items scanned are listed.</p>` : ""}
    <div class="upgrade-list">${items.length ? items.map(item => upgradeRow(item, canStart)).join("") : `<div class="empty">${esc(upgradesEmpty(page))}</div>`}</div>
    ${offset > 0 || page.has_more ? `<div class="row-actions request-filters"><button class="command upgrades-previous" ${offset === 0 ? "disabled" : ""}>Previous</button>
      <button class="command upgrades-next" ${page.has_more ? "" : "disabled"}>Next</button></div>` : ""}`);
  const reload = () => upgradesView();
  bindAll<HTMLButtonElement>(node, ".upgrade-search", button => bindRequest(button,
    `/api/v1/upgrades/${button.dataset.subject}/${button.dataset.id}/search`, { method: "POST" }, "Upgrade search started", reload));
  if (offset > 0 || page.has_more) {
    pick<HTMLButtonElement>(node, ".upgrades-previous").onclick = () => {
      session.upgradesOffset = Math.max(0, offset - upgradesPageSize);
      void upgradesView().catch(showError);
    };
    pick<HTMLButtonElement>(node, ".upgrades-next").onclick = () => {
      session.upgradesOffset = offset + upgradesPageSize;
      void upgradesView().catch(showError);
    };
  }
}
