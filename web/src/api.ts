// Thin fetch wrapper for /api/v1. Cookie auth, CSRF header on writes.

export interface User {
  id: string;
  username: string;
}

/** A Kubernetes-style resource list: cpu, memory and extended resources such as nvidia.com/gpu. */
export type ResourceList = { cpu?: string; memory?: string } & Record<string, string | undefined>;

export interface Resources {
  requests: ResourceList;
  limits: ResourceList;
}

export interface Repo {
  url: string;
  branch?: string;
  path: string;
}

export interface Toleration {
  key?: string;
  operator?: "Equal" | "Exists";
  value?: string;
  effect?: "NoSchedule" | "PreferNoSchedule" | "NoExecute";
  tolerationSeconds?: number;
}

export interface Session {
  id: string;
  name: string;
  agent: string;
  repos: Repo[];
  image_tag: string;
  pvc_size: string;
  storage_class: string;
  runtime_class: string;
  resources: Resources;
  node_selector: Record<string, string>;
  tolerations: Toleration[];
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

/** Chart-wide defaults and ceilings a new session starts from. */
export interface SessionDefaults {
  pvc_size: string;
  storage_class: string;
  image_tag: string;
  autonomous: boolean;
  resources: Resources;
  max_resources: Resources;
  runtime_class: string;
}

export interface Credential {
  kind: string;
  secret: boolean;
  value?: string;
  updated_at: string;
}

export interface GitHubRepo {
  full_name: string;
  clone_url: string;
  ssh_url: string;
  default_branch: string;
  private: boolean;
  archived: boolean;
  description: string;
  pushed_at: string;
  uses: number;
}

export interface CreateSessionRequest {
  name: string;
  agent: string;
  repos: Repo[];
  image_tag?: string;
  pvc_size?: string;
  storage_class?: string;
  runtime_class?: string;
  resources?: Resources;
  node_selector?: Record<string, string>;
  tolerations?: Toleration[];
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

  sessions: () => request<{ sessions: Session[]; agents: string[]; defaults?: SessionDefaults }>("GET", "/sessions"),
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
  githubRepos: (refresh = false) =>
    request<{ configured: boolean; repos: GitHubRepo[]; error?: string }>("GET", `/me/github/repos${refresh ? "?refresh=1" : ""}`),
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
