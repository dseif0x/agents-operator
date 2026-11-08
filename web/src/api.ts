import { checkForUpdate } from "./update";
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
  /** OpenCode only: billion-context compresses the context instead of OpenCode's compaction. */
  context_compression: boolean;
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
  /** Ids of the owner's MCP server accounts and skills the agent gets. */
  mcp_servers: string[];
  skills: string[];
  /** "conversation": the agent is driven through its protocol and shown as a chat; "terminal": a terminal. */
  interface: "conversation" | "terminal";
  /** Conversation sessions: model, context use and the running turn. */
  conversation?: ConvSummary;
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

export interface ConvSummary {
  model?: string;
  ref?: string;
  context?: number;
  context_window?: number;
  /** The running turn; absent when the agent is idle. */
  turn?: string;
  turn_since?: string;
  last_turn_at?: string;
  /** Why the agent CLI does not start. */
  error?: string;
  /** Models the owner can switch to. */
  models?: { id: string; name?: string }[];
  /** What the agent left running in the background (commands, watchers). */
  background?: { id: string; title: string }[];
  input_tokens?: number;
  output_tokens?: number;
  cost_usd?: number;
}

export interface ToolInfo {
  name: string;
  kind: "command" | "edit" | "read" | "search" | "web" | "task" | "mcp" | "other";
  title?: string;
  input?: string;
  output?: string;
  error?: boolean;
}

export interface ItemData {
  text?: string;
  tool?: ToolInfo;
  parent?: string;
  source?: string;
  /** The input id of a message to the agent. */
  input?: string;
  files?: { id: string; name: string; content_type: string; size: number }[];
  /** Routine runs: extra.routine = { id, name, run, source, instruction, event }; extra.quiet when it had no news. */
  request?: AgentRequest;
  response?: { option?: string; answers?: Record<string, string>; message?: string };
  extra?: { routine?: { id: string; name: string; run: number; source: string; instruction: string; event?: string }; quiet?: boolean };
}

/** A note an agent keeps across conversations. */
export interface AgentMemory {
  id: string;
  session_id: string;
  text: string;
  source: "agent" | "owner";
  created_at: string;
  updated_at: string;
}

/** Something the agent waits for: a permission or a question. */
export interface AgentRequest {
  id: string;
  kind: "permission" | "question";
  tool?: ToolInfo;
  title?: string;
  options?: { id: string; label: string; kind: "allow" | "allow_always" | "deny" }[];
  questions?: { id: string; header?: string; question: string; options?: string[]; multi?: boolean }[];
}

/** One item of a conversation: a message to the agent, part of its answer, a tool call, an error. */
export interface ConvItem {
  id: number;
  session_id: string;
  thread: string;
  turn?: string;
  key: string;
  kind: "user" | "text" | "thinking" | "tool" | "error" | "notice" | "request" | "files";
  status: "queued" | "running" | "done" | "failed" | "interrupted" | "deleted" | "pending" | "answered" | "cancelled";
  data: ItemData;
  created_at: string;
  updated_at: string;
}

export interface Schedule {
  kind: "once" | "hourly" | "every_n_hours" | "daily" | "weekdays" | "weekly" | "monthly" | "yearly" | "cron";
  at?: string;
  days?: number[];
  day?: number;
  month?: number;
  every?: number;
  date?: string;
  cron?: string;
  tz?: string;
}

export interface RoutineRun {
  id: number;
  routine_id: string;
  source: "schedule" | "webhook" | "test";
  status: "queued" | "running" | "succeeded" | "failed" | "interrupted" | "sent";
  quiet?: boolean;
  error?: string;
  input_id?: string;
  scheduled_for?: string;
  started_at: string;
  ended_at?: string;
}

export interface Routine {
  id: string;
  session_id?: string;
  chat_id?: string;
  name: string;
  instruction: string;
  trigger: "schedule" | "webhook";
  schedule?: Schedule;
  describe: string;
  enabled: boolean;
  event_type?: string;
  filters?: { path: string; value: string }[];
  webhook_path?: string;
  /** Only right after it was made. */
  secret?: string;
  has_secret?: boolean;
  next_run_at?: string;
  last_run_at?: string;
  last_run?: RoutineRun;
  created_at: string;
}

export interface RoutineInput {
  session_id?: string;
  chat_id?: string;
  name?: string;
  instruction?: string;
  trigger?: "schedule" | "webhook";
  schedule?: Schedule;
  enabled?: boolean;
  event_type?: string;
  filters?: { path: string; value: string }[];
}

export interface CalendarEntry {
  routine_id: string;
  at: string;
  run?: RoutineRun;
}

export interface Activity {
  state: "thinking" | "tool" | "needs_permission" | "waiting_input" | "error" | "exited";
  detail?: string;
  message?: string;
  since: string;
}

export type K8sAccess = "off" | "readonly" | "namespace" | "sandbox";

export type SessionState = "creating" | "running" | "stopping" | "restarting" | "stopped" | "failed" | "deleting";

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
/** A skill of the owner's library: instructions an agent loads when they fit the task (SKILL.md). */
export interface Skill {
  id: string;
  name: string;
  description: string;
  instructions: string;
  created_at: string;
  updated_at: string;
}

export interface SkillInput {
  name: string;
  description: string;
  instructions: string;
}

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
  context_compression?: boolean;
  interface?: "conversation" | "terminal";
  egress?: string[];
  persona?: string;
  agent_definition?: string;
  k8s_access?: K8sAccess;
  k8s_namespaces?: string[];
  ai_account?: string;
  mcp_servers?: string[];
  skills?: string[];
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
  restartSession: (id: string) => request<Session>("POST", `/sessions/${id}/restart`),
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
  skills: () => request<{ skills: Skill[] }>("GET", "/me/skills"),
  createSkill: (s: SkillInput) => request<Skill>("POST", "/me/skills", s),
  updateSkill: (id: string, s: SkillInput) => request<Skill>("PUT", `/me/skills/${id}`, s),
  deleteSkill: (id: string) => request<{ ok: boolean }>("DELETE", `/me/skills/${id}`),
  profile: () => request<Profile>("GET", "/me/profile"),
  putProfile: (p: Profile) => request<Profile>("PUT", "/me/profile", p),
  githubRepos: (refresh = false) =>
    request<{ configured: boolean; repos: GitHubRepo[]; error?: string }>("GET", `/me/github/repos${refresh ? "?refresh=1" : ""}`),

  pushKey: () => request<{ public_key: string }>("GET", "/push/key"),
  pushSubscribe: (sub: PushSubscriptionJSON) => request<{ ok: boolean }>("POST", "/push/subscriptions", sub),
  pushUnsubscribe: (endpoint: string) => request<{ ok: boolean }>("DELETE", "/push/subscriptions", { endpoint }),
  pushTest: () => request<{ sent: number }>("POST", "/push/test"),
  presence: (path: string) => request<{ ok: boolean }>("POST", "/presence", { path }),

  chats: () => request<{ chats: Chat[] }>("GET", "/chats"),
  chat: (id: string) => request<Chat>("GET", `/chats/${id}`),
  createChat: (name: string, members: string[], description = "") => request<Chat>("POST", "/chats", { name, members, description }),
  updateChat: (id: string, patch: { name?: string; description?: string; muted?: boolean }) => request<Chat>("PATCH", `/chats/${id}`, patch),
  deleteChat: (id: string) => request<{ ok: boolean }>("DELETE", `/chats/${id}`),
  addChatMember: (id: string, sessionId: string) => request<Chat>("POST", `/chats/${id}/members`, { session_id: sessionId }),
  removeChatMember: (id: string, sessionId: string) => request<Chat>("DELETE", `/chats/${id}/members/${sessionId}`),
  muteChatMember: (id: string, sessionId: string, muted: boolean) => request<Chat>("PATCH", `/chats/${id}/members/${sessionId}`, { muted }),
  chatMessages: (id: string, opts: { before?: number; after?: number; limit?: number } = {}) => {
    const q = new URLSearchParams();
    if (opts.before) q.set("before", String(opts.before));
    if (opts.after) q.set("after", String(opts.after));
    if (opts.limit) q.set("limit", String(opts.limit));
    return request<{ messages: ChatMessage[] }>("GET", `/chats/${id}/messages?${q}`);
  },
  turnItems: (id: string, turn: string) => request<{ items: ConvItem[] }>("GET", `/sessions/${id}/items?turn=${encodeURIComponent(turn)}`),
  items: (id: string, before = 0, limit = 200) => request<{ items: ConvItem[] }>("GET", `/sessions/${id}/items?before=${before}&limit=${limit}`),
  /** A message to a conversation session's agent; with files it goes as multipart. */
  sendInput: async (id: string, text: string, files: File[] = []): Promise<ConvItem> => {
    if (!files.length) return request<ConvItem>("POST", `/sessions/${id}/inputs`, { text });
    const form = new FormData();
    form.set("text", text);
    for (const f of files) form.append("files", f, f.name || "file");
    const res = await fetch(`/api/v1/sessions/${id}/inputs`, {
      method: "POST",
      headers: csrf ? { "X-CSRF-Token": csrf, Accept: "application/json" } : { Accept: "application/json" },
      credentials: "same-origin",
      body: form,
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new ApiError(res.status, data.error || res.statusText);
    return data as ConvItem;
  },
  editInput: (id: string, input: string, text: string) => request<ConvItem>("PATCH", `/sessions/${id}/inputs/${input}`, { text }),
  sendNow: (id: string, input: string) => request<{ ok: boolean }>("POST", `/sessions/${id}/inputs/${input}/now`),
  cancelInput: (id: string, input: string) => request<{ ok: boolean }>("DELETE", `/sessions/${id}/inputs/${input}`),
  background: (id: string) => request<{ ok: boolean }>("POST", `/sessions/${id}/background`),
  interrupt: (id: string) => request<{ ok: boolean }>("POST", `/sessions/${id}/interrupt`),
  answerRequest: (id: string, requestId: string, r: { option?: string; answers?: Record<string, string>; message?: string }) =>
    request<ConvItem>("POST", `/sessions/${id}/requests/${encodeURIComponent(requestId)}`, r),
  memories: (id: string) => request<{ memories: AgentMemory[] }>("GET", `/sessions/${id}/memories`),
  addMemory: (id: string, text: string) => request<AgentMemory>("POST", `/sessions/${id}/memories`, { text }),
  updateMemory: (id: string, mem: string, text: string) => request<AgentMemory>("PATCH", `/sessions/${id}/memories/${mem}`, { text }),
  deleteMemory: (id: string, mem: string) => request<{ ok: boolean }>("DELETE", `/sessions/${id}/memories/${mem}`),
  setModel: (id: string, model: string) => request<{ ok: boolean }>("POST", `/sessions/${id}/model`, { model }),
  routines: (q: { session?: string; chat?: string } = {}) =>
    request<{ routines: Routine[] }>("GET", `/routines?session=${encodeURIComponent(q.session ?? "")}&chat=${encodeURIComponent(q.chat ?? "")}`),
  createRoutine: (r: RoutineInput) => request<Routine>("POST", "/routines", r),
  updateRoutine: (id: string, r: RoutineInput) => request<Routine>("PATCH", `/routines/${id}`, r),
  deleteRoutine: (id: string) => request<{ ok: boolean }>("DELETE", `/routines/${id}`),
  runRoutine: (id: string) => request<RoutineRun>("POST", `/routines/${id}/run`),
  rotateSecret: (id: string) => request<Routine>("POST", `/routines/${id}/secret`),
  routineRuns: (id: string, limit = 30) => request<{ runs: RoutineRun[] }>("GET", `/routines/${id}/runs?limit=${limit}`),
  routineCalendar: (from: Date, to: Date) => request<{ entries: CalendarEntry[] }>("GET", `/routines/calendar?from=${from.toISOString()}&to=${to.toISOString()}`),
  markChatRead: (id: string, lastId: number) => request<{ ok: boolean }>("POST", `/chats/${id}/read`, { last_id: lastId }),
  /** Post as the owner; with files it goes as multipart. */
  postChatMessage: async (id: string, text: string, replyTo: number, images: File[]): Promise<ChatMessage> => {
    if (!images.length) return request<ChatMessage>("POST", `/chats/${id}/messages`, { text, reply_to: replyTo || 0 });
    const form = new FormData();
    form.set("text", text);
    if (replyTo) form.set("reply_to", String(replyTo));
    for (const f of images) form.append("files", f, f.name || "file");
    const res = await fetch(`/api/v1/chats/${id}/messages`, {
      method: "POST",
      headers: csrf ? { "X-CSRF-Token": csrf, Accept: "application/json" } : { Accept: "application/json" },
      credentials: "same-origin",
      body: form,
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new ApiError(res.status, data.error || res.statusText);
    return data as ChatMessage;
  },
};

export interface ChatMember {
  session_id: string;
  name: string;
  handle: string;
  agent: string;
  persona: string;
  state: SessionState;
  /** What the agent is doing right now (hook activity), when known. */
  activity?: string;
  /** Its detail: the tool, or the permission asked for. */
  activity_detail?: string;
  muted: boolean;
}

export interface ChatAttachment {
  id: string;
  name: string;
  content_type: string;
  size: number;
  url: string;
}

export interface ChatMessage {
  id: number;
  chat_id: string;
  author: { kind: "owner" | "session"; session_id?: string; name: string; handle?: string; deleted?: boolean };
  text: string;
  /** Session ids mentioned (or replied to). */
  mentions: string[];
  everyone: boolean;
  reply_to?: number;
  attachments: ChatAttachment[];
  /** Recipient session id -> "queued" | "delivered". */
  deliveries: Record<string, string>;
  /** The conversation turn an agent's message came from (its steps: items?turn=). */
  turn?: string;
  created_at: string;
}

export interface Chat {
  id: string;
  name: string;
  description: string;
  muted: boolean;
  members: ChatMember[];
  last_message?: ChatMessage;
  unread: number;
  created_at: string;
  updated_at: string;
}

// ---- the event stream ----
// One EventSource for the whole app; every subscriber listens on it.

type Listener = (data: unknown) => void;
const listeners = new Map<string, Set<Listener>>();
const openListeners = new Set<() => void>();
let source: EventSource | null = null;
let retry = 1000;
let subscribers = 0;
// reconnecting marks a connection after an error (the hub went away).
let reconnecting = false;

function connect() {
  if (source || subscribers === 0) return;
  const es = new EventSource("/api/v1/sessions/events");
  source = es;
  es.onopen = () => {
    if (retry > 1000 || reconnecting) checkForUpdate(true);
    reconnecting = false;
    retry = 1000;
    openListeners.forEach((fn) => fn());
  };
  for (const type of ["session", "deleted", "chat", "chat_message", "item"]) {
    es.addEventListener(type, (ev: MessageEvent) => {
      let data: unknown;
      try {
        data = JSON.parse(ev.data);
      } catch {
        return; // ignore malformed frames
      }
      listeners.get(type)?.forEach((fn) => fn(data));
    });
  }
  es.onerror = () => {
    es.close();
    if (source === es) source = null;
    reconnecting = true;
    setTimeout(connect, retry);
    retry = Math.min(retry * 2, 30000);
  };
}

/** Listen to events of the given types; onOpen runs on every (re)connect. Returns a stop function. */
export function onEvents(handlers: Record<string, Listener>, onOpen?: () => void): () => void {
  for (const [type, fn] of Object.entries(handlers)) {
    if (!listeners.has(type)) listeners.set(type, new Set());
    listeners.get(type)!.add(fn);
  }
  if (onOpen) openListeners.add(onOpen);
  subscribers++;
  connect();
  if (source && source.readyState === EventSource.OPEN) onOpen?.();
  return () => {
    for (const [type, fn] of Object.entries(handlers)) listeners.get(type)?.delete(fn);
    if (onOpen) openListeners.delete(onOpen);
    subscribers--;
    if (subscribers === 0) {
      source?.close();
      source = null;
    }
  };
}

/** Chat events from the shared event stream: a chat changed (null: deleted) or a message arrived or changed. */
export function subscribeChats(onChat: (id: string, chat: Chat | null) => void, onMessage: (msg: ChatMessage) => void, onOpen?: () => void): () => void {
  return onEvents(
    {
      chat: (d) => {
        const v = d as { id: string; chat?: Chat };
        onChat(v.id, v.chat ?? null);
      },
      chat_message: (d) => {
        const v = d as { message?: ChatMessage };
        if (v.message) onMessage(v.message);
      },
    },
    onOpen,
  );
}

/** Subscribe to session state changes over SSE. Returns a stop function. */
export function subscribeSessions(onEvent: (type: "session" | "deleted", id: string, session?: Session) => void, onOpen?: () => void): () => void {
  return onEvents(
    {
      session: (d) => {
        const v = d as { id: string; session?: Session };
        onEvent("session", v.id, v.session);
      },
      deleted: (d) => onEvent("deleted", (d as { id: string }).id),
    },
    onOpen,
  );
}

/** Conversation items of one session as they change. */
export function subscribeItems(sessionId: string, onItem: (item: ConvItem) => void, onOpen?: () => void): () => void {
  return onEvents(
    {
      item: (d) => {
        const v = d as { id: string; item?: ConvItem };
        if (v.id === sessionId && v.item) onItem(v.item);
      },
    },
    onOpen,
  );
}

export function wsUrl(id: string): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/api/v1/sessions/${id}/ws`;
}
