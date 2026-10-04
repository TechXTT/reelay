import { api, APIError, esc } from "./api.ts";
import { hooks, session } from "./session.ts";
import type { ConfirmOptions, JellyfinUser, QualityProfile } from "./types.ts";

export function pick<T extends Element>(root: ParentNode, selector: string) {
  return root.querySelector<T>(selector)!;
}

export function bindAll<T extends Element>(root: ParentNode, selector: string, bind: (element: T) => void) {
  root.querySelectorAll<T>(selector).forEach(bind);
}

export function content(html: string) {
  const node = pick<HTMLElement>(document, "#content");
  node.innerHTML = html;
  return node;
}

export function state(value: string) {
  return `<span class="state state-${esc(value)}">${esc(value.replaceAll("_", " "))}</span>`;
}

export function showToast(message: string) {
  const toast = pick<HTMLElement>(document, "#toast");
  toast.textContent = message;
  toast.className = "show";
  setTimeout(() => toast.className = "", 3000);
}

export function showError(error: unknown) {
  if (error instanceof APIError && error.status === 401) {
    hooks.authRequired(error.message);
    return;
  }
  const toast = document.querySelector<HTMLElement>("#toast");
  if (!toast) return;
  toast.textContent = error instanceof Error ? error.message : String(error);
  toast.className = "show error";
  window.setTimeout(() => toast.className = "", 4500);
}

export function setConnection(status: string) {
  session.connectionStatus = status;
  const element = document.querySelector<HTMLElement>("#connection");
  if (!element) return;
  element.textContent = status === "ok" ? "Connected" : status;
  element.className = `connection ${status}`;
}

export function isEditing() {
  return Boolean(document.querySelector("dialog[open]") ||
    document.activeElement?.matches("input, select, textarea"));
}

export async function runControl(control: HTMLButtonElement | HTMLSelectElement, action: () => Promise<void>,
  recover?: () => void, keepDisabledOnSuccess = false) {
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

// Wires a button to one API call, then a toast and an optional reload.
export function bindRequest(button: HTMLButtonElement, path: string, init: RequestInit, message: string,
  reload?: () => Promise<void>) {
  button.onclick = () => void runControl(button, async () => {
    await api(path, init);
    showToast(message);
    await reload?.();
  });
}

export const jsonRequest = (method: string, body: unknown): RequestInit => ({ method, body: JSON.stringify(body) });

export const pad = (value: number) => String(value).padStart(2, "0");

const dateFormatter = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });
const dateTimeFormatter = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

export const date = (value: string | null | undefined) => value
  ? (value.includes("T") ? dateTimeFormatter : dateFormatter).format(new Date(value))
  : "—";

export function percent(progress: number) {
  return Math.max(0, Math.min(100, progress * 100));
}

export function option(value: string, label: string, selected: string) {
  return `<option value="${esc(value)}" ${value === selected ? "selected" : ""}>${esc(label)}</option>`;
}

export function profileOptions(profiles: QualityProfile[], selected: number) {
  return profiles.map(profile => option(String(profile.id), profile.name, String(selected))).join("");
}

export function userKey(user: JellyfinUser) {
  return `${user.server_id}:${user.user_id}`;
}

export function userOptions(users: JellyfinUser[], selected: string) {
  return users.map(user => option(userKey(user), user.display_name, selected)).join("");
}

export function table(headers: string[], rows: string[][]) {
  return `<div class="table-wrap"><table><thead><tr>${headers.map(h => `<th>${h}</th>`).join("")}</tr></thead><tbody>
    ${rows.length ? rows.map(row => `<tr>${row.map(cell => `<td>${cell}</td>`).join("")}</tr>`).join("") : `<tr><td colspan="${headers.length}" class="empty">No records</td></tr>`}
    </tbody></table></div>`;
}

export function openDialog(html: string, closeSelector: string) {
  const dialog = document.createElement("dialog");
  dialog.className = "preview-dialog";
  dialog.innerHTML = html;
  document.body.append(dialog);
  pick<HTMLButtonElement>(dialog, closeSelector).onclick = () => dialog.close();
  dialog.addEventListener("close", () => dialog.remove(), { once: true });
  return dialog;
}

export function confirmDialog(options: ConfirmOptions) {
  return new Promise<Record<string, boolean> | null>(resolve => {
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
        values[check.id] = pick<HTMLInputElement>(dialog, `[name="${check.id}"]`).checked;
      });
      dialog.remove();
      resolve(confirmed ? values : null);
    }, { once: true });
    dialog.addEventListener("cancel", () => { dialog.returnValue = "cancel"; });
    dialog.showModal();
  });
}
