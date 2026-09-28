// Thin fetch wrapper for /api/v1. Cookie auth, CSRF header on writes.

export interface User {
  id: string;
  username: string;
}

export interface Resources {
  requests: { cpu?: string; memory?: string };
  limits: { cpu?: string; memory?: string };
}

export interface Session {
  id: string;
  name: string;
  agent: string;
  repo_url: string;
  branch: string;
  image_tag: string;
  pvc_size: string;
  storage_class: string;
  resources: Resources;
  node_selector: Record<string, string>;
  tolerations: unknown[];
  env: Record<string, string>;
  autonomous: boolean;
  state: SessionState;
  state_reason: string;
  created_at: string;
  updated_at: string;
  last_attached_at: string | null;
  last_output_at: string | null;
  agent_running?: boolean;
  exit_code?: number;
  needs_attention: boolean;
  tail?: string;
  clients: number;
  runner_error?: string;
  pod_name: string;
}

export type SessionState = "creating" | "running" | "stopping" | "stopped" | "failed" | "deleting";

export interface SessionEvent {
  id: number;
  at: string;
  kind: string;
  message: string;
}

export interface Credential {
  kind: string;
  secret: boolean;
  value?: string;
  updated_at: string;
}

export interface CreateSessionRequest {
  name: string;
  agent: string;
  repo_url: string;
  branch: string;
  image_tag?: string;
  pvc_size?: string;
  storage_class?: string;
  resources?: Resources;
  env?: Record<string, string>;
  autonomous?: boolean;
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

let csrf = "";

export function setCsrf(token: string) {
  csrf = token;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET" && csrf) headers["X-CSRF-Token"] = csrf;
  const res = await fetch("/api/v1" + path, {
    method,
    headers,
    credentials: "same-origin",
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { error: text };
  }
  if (!res.ok) {
    const msg = (data as { error?: string } | null)?.error || res.statusText || `HTTP ${res.status}`;
    throw new ApiError(res.status, msg);
  }
  return data as T;
}

export const api = {
  login: (username: string, password: string) =>
    request<{ user: User; csrf: string }>("POST", "/auth/login", { username, password }),
  logout: () => request<{ ok: boolean }>("POST", "/auth/logout"),
  me: () => request<{ user: User; csrf: string }>("GET", "/auth/me"),

  sessions: () => request<{ sessions: Session[]; agents: string[] }>("GET", "/sessions"),
  session: (id: string) => request<Session>("GET", `/sessions/${id}`),
  createSession: (req: CreateSessionRequest) => request<Session>("POST", "/sessions", req),
  deleteSession: (id: string) => request<Session>("DELETE", `/sessions/${id}`),
  startSession: (id: string) => request<Session>("POST", `/sessions/${id}/start`),
  stopSession: (id: string) => request<Session>("POST", `/sessions/${id}/stop`),
  restartAgent: (id: string) => request<{ ok: boolean }>("POST", `/sessions/${id}/restart-agent`),
  saveLogin: (id: string, kind: string) => request<{ ok: boolean }>("POST", `/sessions/${id}/save-login`, { kind }),
  sessionEvents: (id: string) => request<{ events: SessionEvent[] }>("GET", `/sessions/${id}/events`),
  sessionLogs: async (id: string) => {
    const res = await fetch(`/api/v1/sessions/${id}/logs`, { credentials: "same-origin" });
    const text = await res.text();
    if (!res.ok) {
      try {
        throw new ApiError(res.status, JSON.parse(text).error);
      } catch (e) {
        if (e instanceof ApiError) throw e;
        throw new ApiError(res.status, text);
      }
    }
    return text;
  },

  credentials: () => request<{ credentials: Credential[]; kinds: string[] }>("GET", "/me/credentials"),
  putCredentials: (values: Record<string, string>) =>
    request<{ credentials: Credential[]; kinds: string[] }>("PUT", "/me/credentials", { values }),
  deleteCredential: (kind: string) => request<{ ok: boolean }>("DELETE", `/me/credentials/${kind}`),
};

/** Subscribe to session state changes over SSE. Returns a stop function. */
export function subscribeSessions(
  onEvent: (type: "session" | "deleted", id: string, session?: Session) => void,
  onOpen?: () => void,
): () => void {
  let es: EventSource | null = null;
  let stopped = false;
  let retry = 1000;
  const connect = () => {
    if (stopped) return;
    es = new EventSource("/api/v1/sessions/events");
    es.onopen = () => {
      retry = 1000;
      onOpen?.();
    };
    const handler = (type: "session" | "deleted") => (ev: MessageEvent) => {
      try {
        const data = JSON.parse(ev.data) as { id: string; session?: Session };
        onEvent(type, data.id, data.session);
      } catch {
        /* ignore malformed frames */
      }
    };
    es.addEventListener("session", handler("session"));
    es.addEventListener("deleted", handler("deleted"));
    es.onerror = () => {
      es?.close();
      es = null;
      if (!stopped) {
        setTimeout(connect, retry);
        retry = Math.min(retry * 2, 30000);
      }
    };
  };
  connect();
  return () => {
    stopped = true;
    es?.close();
  };
}

export function wsUrl(id: string): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/api/v1/sessions/${id}/ws`;
}
