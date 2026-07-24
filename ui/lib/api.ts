// api.ts — the thin client for the gateway. The master API token is entered once
// and kept in localStorage; every request carries it as a Bearer header. Calls
// are same-origin (/v1/*), reverse-proxied to the gateway by Caddy.

const TOKEN_KEY = 'tgapi_token';

export function getToken(): string {
  if (typeof window === 'undefined') return '';
  return window.localStorage.getItem(TOKEN_KEY) || '';
}

export function setToken(t: string): void {
  window.localStorage.setItem(TOKEN_KEY, t);
}

export function clearToken(): void {
  window.localStorage.removeItem(TOKEN_KEY);
}

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.status = status;
  }
}

// api issues a request and unwraps the {ok, result} envelope. A 401 or an
// envelope error throws an ApiError the UI can react to (e.g. sign out on 401).
export async function api<T = unknown>(
  method: string,
  path: string,
  body?: unknown
): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      Authorization: 'Bearer ' + getToken(),
      ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  const text = await res.text();
  let data: any = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch {
      /* non-JSON error body */
    }
  }

  if (res.status === 401) {
    throw new ApiError('Invalid token', 401);
  }
  if (!res.ok || (data && data.ok === false)) {
    const msg =
      (data && data.error && data.error.message) || res.statusText || 'Request failed';
    throw new ApiError(msg, res.status);
  }
  return (data && 'result' in data ? data.result : data) as T;
}

// --- registry shapes (mirror internal/store) ---

export interface Worker {
  id: string;
  addr: string;
  last_seen_at: string;
  alive: boolean;
  sessions: number;
}

export interface Session {
  id: string;
  kind: 'user' | 'bot';
  status: string;
  phone?: string;
  label?: string;
  worker_id?: string;
  last_seen_at?: string;
  created_at: string;
  app_id: string;
  app_label?: string;
  proxy_id?: string;
  proxy?: string;
}

export interface App {
  id: string;
  api_id: number;
  label?: string;
  created_at: string;
}

export interface Proxy {
  id: string;
  type: string;
  host: string;
  port: number;
  username?: string;
  label?: string;
  created_at: string;
}

export interface Token {
  id: string;
  name?: string;
  enabled: boolean;
  all_sessions: boolean;
  app_ids: string[];
  session_ids: string[];
  created_at: string;
}
