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
  /** Whether the agent gets the owner's SSH key and HTTPS/GitHub tokens; without them the runner only clones with them. */
  git_credentials: boolean;
  /** Local network destinations the agent may reach: `192.168.1.10`, `192.168.1.0/24`, `192.168.1.10:8080`. */
  egress: string[];
  /** Personality and role, added to the agent's own system prompt (not for shell sessions). */
  persona: string;
  /** Immutable chat handle ("eager_turing"); mentions are @handle. */
  handle: string;
  /** OpenCode only: agent definition (Markdown with front matter) started with --agent. */
  agent_definition: string;
  /** What kubectl in the pod may do: nothing, read the cluster, also write in k8s_namespaces, or own a sandbox cluster. */
  k8s_access: K8sAccess;
  k8s_namespaces: string[];
  /** Claude Code and Codex: the account the session signs in with; "" = the owner's default. */
  ai_account: string;
  /** Sandbox mode: the host namespace of the session's vcluster. */
  sandbox_namespace?: string;
  /** Sandbox mode: the DNS name the sandbox's Ingress hosts live under (absent without a sandbox domain). */
  sandbox_host?: string;
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
  /** What the agent says it is doing, from its hooks (Claude Code); absent until the first event. */
  activity?: Activity;
  clients: number;
  runner_error?: string;
  pod_name: string;
}

export interface Activity {
  state: "thinking" | "tool" | "needs_permission" | "waiting_input" | "error" | "exited";
  detail?: string;
  message?: string;
  since: string;
}

export type K8sAccess = "off" | "readonly" | "namespace" | "sandbox";

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
  /** Name of the read-only runner ServiceAccount sessions may opt into; empty when the chart has none. */
  service_account: string;
  /** Whether sessions may ask for write access in namespaces of their choosing. */
  k8s_namespace_write: boolean;
  /** Whether sessions may get a sandbox cluster (a vcluster of their own). */
  k8s_sandbox: boolean;
  /** Domain the sandbox Ingress hosts live under (<session name>.<domain>); empty without Ingress sync. */
  sandbox_domain: string;
  /** Ranges a session may open on the local network; empty when the hub allows none. */
  egress_allowed: string[];
}

/** An AI or git account. Its value (key, token, SSH key, saved login) is never sent back. */
export interface Account {
  id: string;
  kind: string;
  name: string;
  /** Git accounts: the host (github.com, *.example.com, or * for any other). */
  host?: string;
  base_url?: string;
  /** HTTPS git accounts: the username (empty: x-access-token). */
  username?: string;
  /** OpenAI-compatible accounts: the models offered (empty: the endpoint's list). */
  models: string[];
  /** False when the value is missing from the Secret; sessions skip such an account. */
  has_value: boolean;
  created_at: string;
  updated_at: string;
}

export interface AccountInput {
  kind?: string;
  name?: string;
  host?: string;
  base_url?: string;
  username?: string;
  models?: string[];
  /** The secret; on a change, empty keeps the current one. */
  value?: string;
}

export interface Profile {
  git_name: string;
  git_email: string;
  /** Agent (claude, codex) -> the account its sessions use unless they pick one. */
  defaults: Record<string, string>;
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
  git_credentials?: boolean;
  egress?: string[];
  persona?: string;
  agent_definition?: string;
  k8s_access?: K8sAccess;
  k8s_namespaces?: string[];
  ai_account?: string;
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
  /** Replace a stopped session's settings; same body as create. */
  updateSession: (id: string, req: CreateSessionRequest) => request<Session>("PATCH", `/sessions/${id}`, req),
  deleteSession: (id: string) => request<Session>("DELETE", `/sessions/${id}`),
  startSession: (id: string) => request<Session>("POST", `/sessions/${id}/start`),
  stopSession: (id: string) => request<Session>("POST", `/sessions/${id}/stop`),
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

  accounts: () => request<{ accounts: Account[]; kinds: string[] }>("GET", "/me/accounts"),
  createAccount: (a: AccountInput) => request<{ accounts: Account[]; kinds: string[] }>("POST", "/me/accounts", a),
  updateAccount: (id: string, a: AccountInput) => request<{ accounts: Account[]; kinds: string[] }>("PUT", `/me/accounts/${id}`, a),
  deleteAccount: (id: string) => request<{ accounts: Account[]; kinds: string[] }>("DELETE", `/me/accounts/${id}`),
  profile: () => request<Profile>("GET", "/me/profile"),
  putProfile: (p: Profile) => request<Profile>("PUT", "/me/profile", p),
  githubRepos: (refresh = false) =>
    request<{ configured: boolean; repos: GitHubRepo[]; error?: string }>("GET", `/me/github/repos${refresh ? "?refresh=1" : ""}`),
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
