import "./styles.css";
import { connectEvents, esc, setAuthToken } from "./api.ts";
import { hooks, session } from "./session.ts";
import type { View } from "./types.ts";
import { content, isEditing, pick, setConnection, showError } from "./ui.ts";
import { addView } from "./views/add.ts";
import { dashboard } from "./views/dashboard.ts";
import { discoverView } from "./views/discover.ts";
import { moviesView } from "./views/movies.ts";
import { requestsView } from "./views/requests.ts";
import { seriesDetail, seriesView } from "./views/series.ts";
import { settingsView } from "./views/settings.ts";

const app = pick<HTMLDivElement>(document, "#app");
let eventSource: EventSource | null = null;
let refreshTimer = 0;
let refreshPending = false;

const nav: { id: View; label: string; icon: string }[] = [
  { id: "dashboard", label: "Dashboard", icon: "◫" },
  { id: "discover", label: "Discover", icon: "*" },
  { id: "requests", label: "Requests", icon: "↧" },
  { id: "series", label: "Series", icon: "▤" },
  { id: "movies", label: "Movies", icon: "▶" },
  { id: "add", label: "Add", icon: "+" },
  { id: "settings", label: "Settings", icon: "⚙" }
];

const viewLoaders: Record<View, () => Promise<void>> = {
  dashboard,
  discover: () => discoverView(),
  requests: () => requestsView(),
  series: seriesView,
  movies: moviesView,
  add: addView,
  settings: () => settingsView(connect)
};

function shell() {
  app.innerHTML = `<header class="topbar"><button class="brand" data-view="dashboard" aria-label="Dashboard">
    <span class="brand-mark">R</span><strong>Reelay</strong></button>
    <div id="connection" class="connection">Connecting</div></header>
    <div class="layout"><nav aria-label="Primary navigation">${nav.map(n => `<button data-view="${n.id}" title="${n.label}" aria-label="${n.label}" aria-current="${session.current === n.id ? "page" : "false"}" class="${session.current === n.id ? "active" : ""}">
      <span aria-hidden="true">${n.icon}</span><span>${n.label}</span></button>`).join("")}</nav>
    <main id="content"><div class="loading">Loading</div></main></div>
    <div id="toast" role="status" aria-live="polite"></div>`;
  document.querySelectorAll<HTMLElement>("[data-view]").forEach(el => el.onclick = () => navigate(el.dataset.view as View));
  setConnection(session.connectionStatus);
}

async function navigate(view: View) {
  session.current = view;
  session.selectedSeries = null;
  clearTimeout(refreshTimer);
  refreshTimer = 0;
  shell();
  try {
    await viewLoaders[view]();
  } catch (error) {
    showError(error);
  }
}

function authGate(message: string) {
  eventSource?.close();
  eventSource = null;
  setConnection("Authentication required");
  const node = content(`<div class="auth-gate"><div><span class="brand-mark">R</span>
    <h1>Authentication required</h1><p>${esc(message)}</p></div>
    <form id="auth-form"><label for="auth-token">Bearer token</label>
      <input id="auth-token" name="token" type="password" autocomplete="current-password"
        autocapitalize="none" spellcheck="false" required autofocus>
      <button class="command" type="submit">Connect</button></form></div>`);
  pick<HTMLFormElement>(node, "#auth-form").onsubmit = event => {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    setAuthToken(new FormData(form).get("token") as string);
    connect();
    void navigate(session.current);
  };
}

async function refreshLiveView() {
  if (!["dashboard", "requests", "series", "movies"].includes(session.current) || isEditing()) return;
  if (session.refreshing) {
    refreshPending = true;
    return;
  }
  session.refreshing = true;
  try {
    if (session.current === "series" && session.selectedSeries !== null) await seriesDetail(session.selectedSeries);
    else await viewLoaders[session.current]();
  } catch (error) {
    showError(error);
  } finally {
    session.refreshing = false;
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

hooks.navigate = navigate;
hooks.authRequired = authGate;
shell();
connect();
navigate("dashboard");
