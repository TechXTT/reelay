const tokenKey = "reelay.auth-token";

export class APIError extends Error {
  constructor(message: string, readonly status: number, readonly code: string) {
    super(message);
    this.name = "APIError";
  }
}

export function authToken(): string {
  return localStorage.getItem(tokenKey) ?? "";
}

export function normalizeToken(value: string): string {
  let token = value.trim();
  const configLine = token.match(/^auth_token\s*[:=]\s*(.+)$/i);
  if (configLine) token = configLine[1].trim();
  if ((token.startsWith('"') && token.endsWith('"')) ||
      (token.startsWith("'") && token.endsWith("'"))) {
    token = token.slice(1, -1).trim();
  }
  token = token.replace(/^bearer\s+/i, "").trim();
  if ((token.startsWith('"') && token.endsWith('"')) ||
      (token.startsWith("'") && token.endsWith("'"))) {
    token = token.slice(1, -1).trim();
  }
  return token;
}

export function setAuthToken(value: string): void {
  const token = normalizeToken(value);
  if (token) localStorage.setItem(tokenKey, token);
  else localStorage.removeItem(tokenKey);
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  const token = authToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (init.body) headers.set("Content-Type", "application/json");
  const response = await fetch(path, { ...init, headers });
  if (!response.ok) {
    const payload = await response.json().catch(() => ({}));
    throw new APIError(payload?.error?.message ?? `${response.status} ${response.statusText}`,
      response.status, payload?.error?.code ?? "http_error");
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export const esc = (value: unknown): string => String(value ?? "")
  .replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;")
  .replaceAll('"', "&quot;").replaceAll("'", "&#39;");

export function connectEvents(onEvent: () => void): EventSource {
  const token = authToken();
  const url = `/api/v1/events${token ? `?token=${encodeURIComponent(token)}` : ""}`;
  const source = new EventSource(url);
  ["state_transition", "progress", "queue_control"].forEach(type => source.addEventListener(type, onEvent));
  return source;
}
