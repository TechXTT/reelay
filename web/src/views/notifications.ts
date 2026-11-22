import { api, APIError, esc } from "../api.ts";
import { isMember, session } from "../session.ts";
import type { JellyfinUser, NotificationDestinationResponse } from "../types.ts";
import { date, jsonRequest, pick, runControl, showError, showToast, userKey } from "../ui.ts";

const topicPattern = /^[A-Za-z0-9_-]{1,64}$/;

export function notificationsPanelShell() {
  return `<section class="notify-panel" aria-labelledby="notify-heading"><h2 id="notify-heading">Notifications</h2><p class="notify-note">Loading…</p></section>`;
}

function scopeQuery(user: JellyfinUser) {
  return new URLSearchParams({ server_id: user.server_id, user_id: user.user_id });
}

function currentSummary(info: NotificationDestinationResponse) {
  const destination = info.destination;
  if (!destination) return `<p class="notify-note">No notification destination is set for this user.</p>`;
  const target = destination.kind === "ntfy"
    ? `ntfy topic <strong>${esc(destination.topic)}</strong> on ${esc(info.ntfy_base_url || "the configured ntfy server")}`
    : `webhook <strong>${esc(destination.url)}</strong> (${esc(destination.format || "json")} format)`;
  const delivery = info.last_delivery;
  const last = !delivery ? "No notification has been delivered yet."
    : delivery.error ? `Last delivery failed after ${delivery.attempts} attempt${delivery.attempts === 1 ? "" : "s"}: ${esc(delivery.error)}`
    : delivery.at ? `Last delivered ${esc(date(delivery.at))}.` : "Delivery is pending.";
  return `<p class="notify-current">${target} · ${destination.enabled ? "enabled" : "paused"}</p>
    <p class="notify-note${delivery?.error ? " notify-error" : ""}">${last}</p>`;
}

function formMarkup(info: NotificationDestinationResponse, operator: boolean) {
  const destination = info.destination;
  const ntfyOnly = !operator;
  if (!info.ntfy_available && ntfyOnly) {
    return `<p class="notify-note">Per-user notifications are not configured by the operator.</p>`;
  }
  const kind = destination?.kind === "webhook" && operator ? "webhook" : info.ntfy_available ? "ntfy" : "webhook";
  const kindChoice = operator ? `<label>Destination type<select name="kind" id="notify-kind">
      ${info.ntfy_available ? `<option value="ntfy" ${kind === "ntfy" ? "selected" : ""}>ntfy topic</option>` : ""}
      <option value="webhook" ${kind === "webhook" ? "selected" : ""}>Webhook URL</option></select></label>` : `<input type="hidden" name="kind" value="ntfy">`;
  return `<form class="notify-form" novalidate>
    ${kindChoice}
    <label class="notify-topic-field" ${kind === "ntfy" ? "" : "hidden"}>ntfy topic
      <input name="topic" maxlength="64" pattern="[A-Za-z0-9_\\-]{1,64}" autocomplete="off" autocapitalize="none" spellcheck="false" value="${esc(destination?.topic ?? "")}" aria-describedby="notify-topic-hint">
      <small id="notify-topic-hint">1-64 letters, digits, - or _. Anyone who knows the topic can read it, so pick a hard-to-guess name.</small></label>
    ${operator ? `<label class="notify-webhook-field" ${kind === "webhook" ? "" : "hidden"}>Webhook URL
      <input name="url" type="url" autocomplete="off" spellcheck="false" value="${esc(destination?.url ?? "")}" placeholder="https://"></label>
    <label class="notify-webhook-field" ${kind === "webhook" ? "" : "hidden"}>Payload format
      <select name="format"><option value="json" ${destination?.format !== "ntfy" ? "selected" : ""}>JSON</option><option value="ntfy" ${destination?.format === "ntfy" ? "selected" : ""}>ntfy</option></select></label>` : ""}
    ${operator && !info.ntfy_available ? `<small>Per-user ntfy topics are off. Set <code>availability.user_ntfy_base_url</code> to let members choose a topic.</small>` : ""}
    <label class="notify-switch"><input type="checkbox" name="enabled" role="switch" ${!destination || destination.enabled ? "checked" : ""}><span>Send notifications</span></label>
    <div class="row-actions"><button class="command" type="submit">Save</button>
      <button class="command notify-test" type="button" ${destination ? "" : "disabled"}>Send test</button>
      <button class="command danger notify-remove" type="button" ${destination ? "" : "disabled"}>Remove</button></div>
    <p class="notify-result" role="status" aria-live="polite"></p></form>`;
}

function describeFailure(error: unknown) {
  if (error instanceof APIError && error.status === 429) {
    return error.retryAfter ? `A test was sent recently. Wait ${error.retryAfter} seconds before sending another.` : error.message;
  }
  if (error instanceof APIError && error.status === 502) return `The test notification could not be delivered: ${error.message}`;
  return error instanceof Error ? error.message : String(error);
}

// Never blocks Discover: a failure leaves a message in the panel only.
export async function loadNotificationsPanel(node: HTMLElement, user: JellyfinUser) {
  const panel = pick<HTMLElement>(node, ".notify-panel");
  const key = userKey(user);
  const query = scopeQuery(user);
  const operator = !isMember();
  const stale = () => !panel.isConnected || session.current !== "discover" || session.discoverUser !== key;
  const render = (markup: string) => {
    panel.querySelectorAll(":scope > :not(h2)").forEach(element => element.remove());
    panel.insertAdjacentHTML("beforeend", markup);
  };
  let info: NotificationDestinationResponse;
  try {
    info = await api<NotificationDestinationResponse>(`/api/v1/notifications/destination?${query}`);
  } catch (error) {
    if (stale()) return;
    render(`<p class="notify-note notify-error" role="alert">Notification settings are unavailable: ${esc(error instanceof Error ? error.message : String(error))}</p>`);
    return;
  }
  if (stale()) return;
  render(`${currentSummary(info)}${formMarkup(info, operator)}`);
  const form = panel.querySelector<HTMLFormElement>(".notify-form");
  if (!form) return;
  const result = pick<HTMLElement>(form, ".notify-result");
  const setResult = (message: string, failed = false) => {
    result.textContent = message;
    result.className = `notify-result${failed ? " notify-error" : ""}`;
  };
  const reload = () => loadNotificationsPanel(node, user);
  const kindSelect = form.querySelector<HTMLSelectElement>("#notify-kind");
  if (kindSelect) kindSelect.onchange = () => {
    form.querySelectorAll<HTMLElement>(".notify-topic-field").forEach(el => el.hidden = kindSelect.value !== "ntfy");
    form.querySelectorAll<HTMLElement>(".notify-webhook-field").forEach(el => el.hidden = kindSelect.value !== "webhook");
  };

  form.onsubmit = event => {
    event.preventDefault();
    const data = new FormData(form);
    const kind = String(data.get("kind"));
    const body: Record<string, unknown> = { kind, enabled: data.get("enabled") === "on" };
    if (kind === "ntfy") {
      const topic = String(data.get("topic") ?? "").trim();
      if (!topicPattern.test(topic)) {
        setResult("Topic must be 1-64 characters: letters, digits, - or _.", true);
        form.querySelector<HTMLInputElement>("[name=topic]")?.focus();
        return;
      }
      body.topic = topic;
    } else {
      const url = String(data.get("url") ?? "").trim();
      if (!/^https?:\/\/\S+$/i.test(url)) {
        setResult("Enter an absolute http(s) URL without spaces.", true);
        form.querySelector<HTMLInputElement>("[name=url]")?.focus();
        return;
      }
      body.url = url;
      body.format = String(data.get("format") ?? "json");
    }
    if (operator) Object.assign(body, { server_id: user.server_id, user_id: user.user_id });
    const save = pick<HTMLButtonElement>(form, "[type=submit]");
    setResult("Saving…");
    void runControl(save, async () => {
      try {
        await api("/api/v1/notifications/destination", jsonRequest("PUT", body));
      } catch (error) {
        setResult(describeFailure(error), true);
        throw error;
      }
      showToast("Notification destination saved");
      await reload();
    });
  };

  const remove = pick<HTMLButtonElement>(form, ".notify-remove");
  remove.onclick = () => void runControl(remove, async () => {
    try {
      await api(`/api/v1/notifications/destination?${query}`, { method: "DELETE" });
    } catch (error) {
      setResult(describeFailure(error), true);
      throw error;
    }
    showToast("Notification destination removed");
    await reload();
  });

  const test = pick<HTMLButtonElement>(form, ".notify-test");
  test.onclick = () => {
    setResult("Sending test notification…");
    // Errors are shown inline instead of as a toast.
    void runControl(test, async () => {
      try {
        await api(`/api/v1/notifications/destination/test?${query}`, { method: "POST" });
        setResult("Test notification sent.");
        showToast("Test notification sent");
      } catch (error) {
        if (error instanceof APIError && error.status === 401) throw error;
        setResult(describeFailure(error), true);
        showError(new Error(describeFailure(error)));
      }
    });
  };
}
