import "./styles.css";
import { api, APIError, authToken, connectEvents, esc, setAuthToken } from "./api.ts";
import { hooks, isMember, session } from "./session.ts";
import type { AuthConfig, Principal, View } from "./types.ts";
import { content, isEditing, jsonRequest, pick, runControl, setConnection, showError } from "./ui.ts";
import { addView } from "./views/add.ts";
import { dashboard } from "./views/dashboard.ts";
import { discoverView } from "./views/discover.ts";
import { moviesView } from "./views/movies.ts";
import { requestsView } from "./views/requests.ts";
import { seriesDetail, seriesView } from "./views/series.ts";
import { settingsView } from "./views/settings.ts";
import { tonightView } from "./views/tonight.ts";
import { subtitlesView } from "./views/subtitles.ts";
import { upgradesView } from "./views/upgrades.ts";

const app = pick<HTMLDivElement>(document, "#app");
let eventSource: EventSource | null = null;
let refreshTimer = 0;
let refreshPending = false;

const nav: { id: View; label: string; icon: string }[] = [
  { id: "dashboard", label: "Dashboard", icon: "◫" },
  { id: "discover", label: "Discover", icon: "*" },
  { id: "tonight", label: "Tonight", icon: "☾" },
  { id: "requests", label: "Requests", icon: "↧" },
  { id: "series", label: "Series", icon: "▤" },
  { id: "movies", label: "Movies", icon: "▶" },
  { id: "upgrades", label: "Upgrades", icon: "↑" },
  { id: "subtitles", label: "Subtitles", icon: "≡" },
  { id: "add", label: "Add", icon: "+" },
  { id: "settings", label: "Settings", icon: "⚙" }
];

const memberViews: View[] = ["discover", "tonight", "requests"];
const visibleNav = () => !session.principal ? [] : isMember() ? nav.filter(n => memberViews.includes(n.id)) : nav;

const viewLoaders: Record<View, () => Promise<void>> = {
  dashboard,
  discover: () => discoverView(),
  tonight: () => tonightView(),
  requests: () => requestsView(),
  series: seriesView,
  movies: moviesView,
  upgrades: () => upgradesView(),
  subtitles: () => subtitlesView(),
  add: addView,
  settings: () => settingsView(connect)
};

function identity() {
  const principal = session.principal;
  if (!principal) return "";
  return `<div class="identity"><span class="identity-name">${esc(principal.name)} <small>${esc(principal.role)}</small></span>
    ${principal.session ? `<button class="command compact" id="sign-out">Sign out</button>` : ""}</div>`;
}

function shell() {
  const items = visibleNav();
  const home = isMember() ? "discover" : "dashboard";
  app.innerHTML = `<header class="topbar"><button class="brand" data-view="${home}" aria-label="${isMember() ? "Discover" : "Dashboard"}">
    <span class="brand-mark">R</span><strong>Reelay</strong></button>
    <div class="topbar-status">${isMember() ? "" : `<div id="connection" class="connection">Connecting</div>`}${identity()}</div></header>
    <div class="layout"><nav aria-label="Primary navigation" style="--nav-count:${items.length}">${items.map(n => `<button data-view="${n.id}" title="${n.label}" aria-label="${n.label}" aria-current="${session.current === n.id ? "page" : "false"}" class="${session.current === n.id ? "active" : ""}">
      <span aria-hidden="true">${n.icon}</span><span>${n.label}</span></button>`).join("")}</nav>
    <main id="content"><div class="loading">Loading</div></main></div>
    <div id="toast" role="status" aria-live="polite"></div>`;
  document.querySelectorAll<HTMLElement>("[data-view]").forEach(el => el.onclick = () => navigate(el.dataset.view as View));
  const signOut = document.querySelector<HTMLButtonElement>("#sign-out");
  if (signOut) signOut.onclick = () => void runControl(signOut, signOutSession);
  setConnection(session.connectionStatus);
}

async function signOutSession() {
  await api("/api/v1/auth/logout", jsonRequest("POST", {}));
  await startSignIn("You have been signed out.");
}

async function navigate(view: View) {
  if (!session.principal) return;
  if (isMember() && !memberViews.includes(view)) view = "discover";
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
  void startSignIn(message);
}

function showTokenPrompt(message: string, config: AuthConfig | null) {
  const node = content(`<div class="auth-gate"><div><span class="brand-mark">R</span>
    <h1>Authentication required</h1><p>${esc(message)}</p></div>
    <form id="auth-form"><label for="auth-token">Bearer token</label>
      <input id="auth-token" name="token" type="password" autocomplete="current-password"
        autocapitalize="none" spellcheck="false" required autofocus>
      <button class="command" type="submit">Connect</button></form>
      ${config ? `<button class="link-button" id="use-login" type="button">Sign in with Jellyfin instead</button>` : ""}</div>`);
  pick<HTMLFormElement>(node, "#auth-form").onsubmit = event => {
    event.preventDefault();
    const form = event.currentTarget as HTMLFormElement;
    setAuthToken(new FormData(form).get("token") as string);
    void start();
  };
  if (config) pick<HTMLButtonElement>(node, "#use-login").onclick = () => showLogin(config, "");
}

function showLogin(config: AuthConfig, message: string) {
  const servers = config.servers ?? [];
  const node = content(`<div class="auth-gate"><div><span class="brand-mark">R</span>
    <h1>Sign in</h1><p>${esc(message || "Use your Jellyfin account.")}</p></div>
    <form id="login-form">
      ${servers.length > 1 ? `<label for="login-server">Jellyfin server</label><select id="login-server" name="server_id">${servers.map(id => `<option value="${esc(id)}">${esc(id)}</option>`).join("")}</select>` : ""}
      <label for="login-username">Username</label>
      <input id="login-username" name="username" autocomplete="username" autocapitalize="none" spellcheck="false" required autofocus>
      <label for="login-password">Password</label>
      <input id="login-password" name="password" type="password" autocomplete="current-password" required>
      <p id="login-error" class="auth-error" role="alert" hidden></p>
      <button class="command" type="submit">Sign in</button></form>
    <button class="link-button" id="use-token" type="button">Use operator token instead</button></div>`);
  const form = pick<HTMLFormElement>(node, "#login-form");
  const error = pick<HTMLElement>(node, "#login-error");
  const submit = pick<HTMLButtonElement>(form, "[type=submit]");
  form.onsubmit = event => {
    event.preventDefault();
    const data = new FormData(form);
    error.hidden = true;
    submit.textContent = "Signing in…";
    void runControl(submit, async () => {
      try {
        await api("/api/v1/auth/login", jsonRequest("POST", {
          server_id: (data.get("server_id") as string | null) ?? "",
          username: data.get("username"), password: data.get("password")
        }));
      } catch (caught) {
        // 401, 403 and 429 carry user-facing messages from the API.
        error.textContent = caught instanceof Error ? caught.message : String(caught);
        error.hidden = false;
        submit.textContent = "Sign in";
        return;
      }
      // A stored bearer token would take precedence over the new session cookie.
      setAuthToken("");
      await start();
    });
  };
  pick<HTMLButtonElement>(node, "#use-token").onclick = () =>
    showTokenPrompt("Paste the operator bearer token.", config);
}

// Shows the sign-in screen when household logins are enabled, otherwise the bearer-token prompt.
async function startSignIn(message: string) {
  eventSource?.close();
  eventSource = null;
  session.principal = null;
  shell();
  setConnection("Authentication required");
  let config: AuthConfig = { logins_enabled: false, servers: [] };
  try {
    config = await api<AuthConfig>("/api/v1/auth/config");
  } catch {
    // A server without the endpoint behaves as if logins are disabled.
  }
  if (config.logins_enabled) showLogin(config, message);
  else showTokenPrompt(message, null);
}

async function refreshLiveView() {
  const live = isMember() ? ["requests"] : ["dashboard", "requests", "series", "movies", "upgrades"];
  if (!live.includes(session.current) || isEditing()) return;
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
  eventSource = null;
  // Members cannot open the operator-only event stream.
  if (isMember()) return;
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

// Members have no event stream, so Requests refreshes itself while the tab is visible.
window.setInterval(() => {
  if (isMember() && !document.hidden && session.current === "requests") void refreshLiveView();
}, 60000);

// Resolves who is signed in (stored token or session cookie), then starts the app or shows the sign-in screen.
async function start() {
  try {
    session.principal = await api<Principal>("/api/v1/auth/me");
  } catch (error) {
    if (error instanceof APIError && error.status === 401) {
      await startSignIn(authToken() ? error.message : "Sign in to continue.");
    } else {
      shell();
      showError(error);
    }
    return;
  }
  session.current = isMember() ? "discover" : "dashboard";
  shell();
  connect();
  await navigate(session.current);
}

void start();
