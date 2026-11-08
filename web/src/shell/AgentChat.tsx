// An agent's own conversation: your messages, its answers, its work steps
// folded per turn, the queue, and the composer.
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import { createPortal } from "preact/compat";
import { api, subscribeItems, type ConvItem, type Session } from "../api";
import { Link, navigate } from "../router";
import { copyCode, renderMessage } from "../markdown";
import { Avatar } from "./Avatar";
import { Icon } from "./icons";
import { agentLabel, agentStatus, longTime, shortTime } from "./format";
import { RoutineList } from "./Routines";
import { loadDraft, saveDraft } from "../drafts";

type Block =
  | { type: "user"; item: ConvItem }
  | { type: "text"; item: ConvItem }
  | { type: "steps"; items: ConvItem[]; key: string }
  | { type: "error"; item: ConvItem }
  | { type: "notice"; item: ConvItem }
  | { type: "date"; key: string; at: string }
  | { type: "routine"; item: ConvItem; folded: ConvItem[] }
  | { type: "request"; item: ConvItem }
  | { type: "tasks"; item: ConvItem; key: string };

const PAGE = 200;

function draftKey(id: string) {
  return `agents-operator.draft.${id}`;
}

export function AgentChat({ session }: { session: Session }) {
  const id = session.id;
  const [items, setItems] = useState<ConvItem[] | null>(null);
  const [hasMore, setHasMore] = useState(false);
  const [error, setError] = useState("");
  const [panel, setPanel] = useState(false);
  const [slot, setSlot] = useState<HTMLElement | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const stick = useRef(true);

  const upsert = (it: ConvItem) =>
    setItems((cur) => {
      if (!cur) return cur;
      const i = cur.findIndex((x) => x.key === it.key);
      if (it.status === "deleted") return i < 0 ? cur : cur.filter((_, j) => j !== i);
      if (i >= 0) {
        if (new Date(cur[i].updated_at) > new Date(it.updated_at) && cur[i].id === it.id) return cur;
        const next = cur.slice();
        next[i] = it;
        return next;
      }
      const next = [...cur, it];
      next.sort((a, b) => a.id - b.id);
      return next;
    });

  const load = () =>
    api
      .items(id, 0, PAGE)
      .then((r) => {
        setItems(r.items);
        setHasMore(r.items.length >= PAGE);
        setError("");
      })
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    setSlot(document.getElementById("sh-topbar-slot"));
    load();
    return subscribeItems(id, upsert, load);
  }, [id]);

  const loadOlder = async () => {
    if (!items?.length) return;
    const el = scroller.current;
    const before = el ? el.scrollHeight - el.scrollTop : 0;
    const r = await api.items(id, items[0].id, PAGE);
    setHasMore(r.items.length >= PAGE);
    setItems((cur) => [...r.items, ...(cur ?? [])]);
    requestAnimationFrame(() => {
      if (el) el.scrollTop = el.scrollHeight - before;
    });
  };

  // Follow the end of the conversation while the reader is there.
  useLayoutEffect(() => {
    const el = scroller.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [items, session.conversation?.turn]);
  const onScroll = () => {
    const el = scroller.current;
    if (el) stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  };

  const queued = (items ?? []).filter((it) => it.kind === "user" && it.status === "queued");
  const blocks = useMemo(() => toBlocks((items ?? []).filter((it) => !(it.kind === "user" && it.status === "queued"))), [items]);
  const conv = session.conversation ?? {};
  const working = Boolean(conv.turn) && session.state === "running";
  // A command runs in the foreground that Claude Code can move to the
  // background (its Ctrl+B).
  const foreground =
    working && session.agent === "claude" && (items ?? []).some((it) => it.kind === "tool" && it.status === "running" && it.data.tool?.kind === "command" && !it.data.parent);
  const status = agentStatus(session);

  const header = (
    <>
      <span class="sh-title">
        <Avatar id={id} size={22} status={status} />
        <span>{session.name}</span>
      </span>
      {(conv.background?.length ?? 0) > 0 && <BackgroundChip items={conv.background!} />}
      {conv.model && <ModelPicker session={session} />}
      <ContextRing used={conv.context} size={conv.context_window} />
      <SessionMenu session={session} />
      <button class="sh-icon-btn" title="Details" onClick={() => setPanel((p) => !p)}>
        <Icon name="panel" size={18} />
      </button>
    </>
  );

  return (
    <div style={{ display: "flex", flex: 1, minHeight: 0 }}>
      {slot && createPortal(header, slot)}
      <div style={{ display: "flex", flexDirection: "column", flex: 1, minWidth: 0 }}>
        {session.state !== "running" && <StateBanner session={session} />}
        {session.state === "running" && conv.error && (
          <div class="cv-banner bad">
            <Icon name="alert" size={18} />
            <span class="text">
              {conv.error}
              <br />
              <span style={{ color: "var(--sh-muted)" }}>If the CLI needs a login, run it in the terminal (for example `claude /login`).</span>
            </span>
            <Link href={`/sessions/${id}/terminal`} class="sh-btn small">
              <Icon name="terminal" size={15} /> Terminal
            </Link>
          </div>
        )}
        <div
          class="cv-scroll"
          ref={scroller}
          onScroll={onScroll}
          onClick={(e) => {
            const copy = (e.target as HTMLElement).closest<HTMLElement>(".code-copy");
            if (copy) {
              e.preventDefault();
              copyCode(copy);
            }
          }}
        >
          <div class="cv-inner">
            {error && <div class="cv-error">{error}</div>}
            {items === null && !error && <div class="cv-notice">Loading…</div>}
            {hasMore && (
              <button class="sh-btn small cv-load-more" onClick={loadOlder}>
                Load earlier messages
              </button>
            )}
            {items?.length === 0 && (
              <div class="sh-placeholder" style={{ marginTop: "12vh" }}>
                <Avatar id={id} size={64} status={status} />
                <h2>{session.name}</h2>
                {session.persona ? session.persona.split("\n")[0] : `${agentLabel[session.agent] ?? session.agent} in its own workspace.`}
                <div style={{ marginTop: 6, fontSize: 12.5 }}>@{session.handle}</div>
              </div>
            )}
            {blocks.map((b) => (
              <BlockView key={b.type === "steps" || b.type === "date" || b.type === "tasks" ? b.key : b.item.key} b={b} session={session} />
            ))}
            {working && <Working session={session} />}
          </div>
        </div>
        <Bottom session={session} queued={queued} working={working} foreground={foreground} onSent={() => (stick.current = true)} />
      </div>
      {panel && <InfoPanel session={session} onClose={() => setPanel(false)} />}
    </div>
  );
}

/** Groups items into what the timeline shows. */
function toBlocks(items: ConvItem[]): Block[] {
  const out: Block[] = [];
  let steps: ConvItem[] | null = null;
  let lastDay = "";
  let lastAt = 0;
  let quietTurn: { turn: string; block: Block } | null = null;
  for (const it of items) {
    const at = new Date(it.created_at);
    const day = at.toDateString();
    if (day !== lastDay || at.getTime() - lastAt > 3 * 3600_000) {
      out.push({ type: "date", key: `date-${it.id}`, at: it.created_at });
      lastDay = day;
      steps = null;
    }
    lastAt = at.getTime();
    if (quietTurn && it.turn && it.turn === quietTurn.turn && it.kind !== "user") {
      (quietTurn.block as { folded: ConvItem[] }).folded.push(it);
      continue;
    }
    if (it.kind === "request") {
      steps = null;
      out.push({ type: "request", item: it });
      continue;
    }
    if (it.kind === "tool" && taskList(it)) {
      // The newest task list of a run of steps replaces the one before.
      const prev = out.findIndex((b) => b.type === "tasks" && b.item.turn === it.turn);
      if (prev >= 0) out.splice(prev, 1);
      out.push({ type: "tasks", item: it, key: `tasks-${it.turn}` });
      steps = null;
      continue;
    }
    const isStep = it.kind === "tool" || it.kind === "thinking" || (it.kind === "text" && it.data.parent);
    if (isStep) {
      if (!steps || (steps[0].turn !== it.turn && it.turn)) {
        steps = [];
        out.push({ type: "steps", items: steps, key: `steps-${it.key}` });
      }
      steps.push(it);
      continue;
    }
    steps = null;
    const rt = it.kind === "user" ? it.data.extra?.routine : undefined;
    if (rt) {
      const block: Block = { type: "routine", item: it, folded: [] };
      out.push(block);
      if (it.data.extra?.quiet) quietTurn = { turn: it.turn ?? "", block };
      continue;
    }
    if (quietTurn && it.turn && it.turn === quietTurn.turn) {
      // A routine run with no news: its answer stays folded in the marker.
      (quietTurn.block as { folded: ConvItem[] }).folded.push(it);
      continue;
    }
    if (it.kind === "user") out.push({ type: "user", item: it });
    else if (it.kind === "text") out.push({ type: "text", item: it });
    else if (it.kind === "error") out.push({ type: "error", item: it });
    else out.push({ type: "notice", item: it });
  }
  return out;
}

function BlockView({ b, session }: { b: Block; session: Session }) {
  switch (b.type) {
    case "date":
      return <div class="cv-date">{longTime(b.at)}</div>;
    case "user":
      return <UserMessage item={b.item} session={session} />;
    case "routine":
      return <RoutineMarker item={b.item} folded={b.folded} />;
    case "request":
      return <RequestCard item={b.item} session={session} />;
    case "tasks":
      return <TaskCard item={b.item} />;
    case "text":
      return (
        <div class="cv-agent">
          <div class={`cv-bubble cv-md ${b.item.status === "running" ? "cv-cursor" : ""}`} dangerouslySetInnerHTML={{ __html: renderMessage(b.item.data.text ?? "", []) }} />
        </div>
      );
    case "steps":
      return <Steps items={b.items} />;
    case "error":
      return (
        <div class="cv-error">
          <Icon name="alert" size={16} />
          <span>{b.item.data.text}</span>
        </div>
      );
    case "notice":
      return (
        <div class="cv-notice">
          <Icon name={b.item.data.text === "Stopped" ? "stop" : "refresh"} size={13} />
          {b.item.data.text}
        </div>
      );
  }
}

function RoutineMarker({ item, folded }: { item: ConvItem; folded: ConvItem[] }) {
  const [open, setOpen] = useState(false);
  const rt = item.data.extra!.routine!;
  const quiet = item.data.extra?.quiet;
  const src = rt.source === "webhook" ? `webhook${rt.event ? " " + rt.event : ""}` : rt.source === "test" ? "test run" : "scheduled";
  return (
    <div class="cv-routine">
      <button class="cv-routine-head" onClick={() => setOpen((o) => !o)}>
        <Icon name={rt.source === "webhook" ? "plug" : "clock"} size={14} class={item.status === "queued" ? "" : "cv-ok"} />
        <b>{rt.name}</b>
        <span>· {src}</span>
        {item.status === "queued" && <span>· waiting</span>}
        {quiet && <span>· no update</span>}
        <span>· {shortTime(item.created_at)}</span>
        <Icon name={open ? "chevronDown" : "chevronRight"} size={13} />
      </button>
      {open && (
        <div class="cv-routine-body">
          <div class="cv-routine-instr">{rt.instruction}</div>
          {folded.filter((f) => f.kind === "text").map((f) => (
            <div class="cv-bubble cv-md" dangerouslySetInnerHTML={{ __html: renderMessage(f.data.text ?? "", []) }} />
          ))}
        </div>
      )}
    </div>
  );
}

function UserMessage({ item, session }: { item: ConvItem; session: Session }) {
  const [open, setOpen] = useState(false);
  const fromChat = item.data.source === "chat";
  const files = item.data.files ?? [];
  return (
    <div class={`cv-user ${fromChat ? "from-chat" : ""} ${open ? "open" : ""}`}>
      {fromChat && (
        <span class="cv-source">
          <Icon name="chats" size={12} /> From a chat
        </span>
      )}
      {files.length > 0 && (
        <div class="cv-files">
          {files.map((f) =>
            f.content_type.startsWith("image/") ? (
              <a class="cv-file image" href={`/api/v1/sessions/${session.id}/files/${f.id}`} target="_blank" rel="noopener">
                <img src={`/api/v1/sessions/${session.id}/files/${f.id}`} alt={f.name} />
              </a>
            ) : (
              <a class="cv-file" href={`/api/v1/sessions/${session.id}/files/${f.id}`}>
                <Icon name="file" size={16} />
                <span>{f.name}</span>
              </a>
            ),
          )}
        </div>
      )}
      {item.data.text && (
        <div class="cv-bubble" onClick={() => fromChat && setOpen((o) => !o)}>
          {item.data.text}
        </div>
      )}
    </div>
  );
}

const toolIcon: Record<string, string> = { command: "terminal", edit: "pencil", read: "eye", search: "search", web: "globe", task: "tasks", mcp: "plug", other: "wrench" };
const toolVerb: Record<string, string> = { command: "Ran", edit: "Edited", read: "Read", search: "Searched", web: "Fetched", task: "", mcp: "Used", other: "Used" };

function stepTitle(it: ConvItem): string {
  if (it.kind === "thinking") return it.status === "running" ? "Thinking" : "Thought";
  if (it.kind === "text") return (it.data.text ?? "").split("\n")[0].slice(0, 120) || "Sub-agent";
  const t = it.data.tool;
  return t?.title || t?.name || "Tool";
}

/** The work behind an agent's chat message: the steps of its turn. */
export function TurnSteps({ sessionId, turn }: { sessionId: string; turn: string }) {
  const [items, setItems] = useState<ConvItem[] | null>(null);
  useEffect(() => {
    let live = true;
    api
      .turnItems(sessionId, turn)
      .then((r) => live && setItems(r.items.filter((it) => it.kind === "tool" || it.kind === "thinking")))
      .catch(() => live && setItems([]));
    return () => {
      live = false;
    };
  }, [sessionId, turn]);
  if (!items?.length) return null;
  return <Steps items={items} />;
}

function Steps({ items }: { items: ConvItem[] }) {
  const running = items.find((it) => it.status === "running");
  const [open, setOpen] = useState(false);
  const failed = items.filter((it) => it.status === "failed").length;
  const tools = items.filter((it) => it.kind === "tool");
  const first = new Date(items[0].created_at).getTime();
  const last = Math.max(...items.map((it) => new Date(it.updated_at).getTime()));
  const secs = Math.max(0, Math.round((last - first) / 1000));
  const duration = secs >= 60 ? `${Math.floor(secs / 60)}m ${secs % 60}s` : `${secs}s`;
  let label: string;
  if (running) label = running.kind === "tool" ? `${toolVerb[running.data.tool?.kind ?? "other"] ? "Running" : ""} ${stepTitle(running)}`.trim() : stepTitle(running);
  else if (tools.length === 0) label = "Thought it through";
  else label = `Worked through ${tools.length} step${tools.length === 1 ? "" : "s"}`;
  return (
    <div class="cv-steps">
      <button class="cv-steps-head" onClick={() => setOpen((o) => !o)}>
        {running ? <span class="cv-spin" /> : failed ? <Icon name="alert" size={16} class="cv-bad" /> : <Icon name="check" size={16} class="cv-ok" />}
        <span class="label">{label}</span>
        <span class="count">{running ? "" : duration}</span>
        <Icon name={open ? "chevronDown" : "chevronRight"} size={15} />
      </button>
      {open && (
        <div class="cv-step-list">
          {items.map((it) => (
            <Step key={it.key} it={it} />
          ))}
        </div>
      )}
    </div>
  );
}

function Step({ it }: { it: ConvItem }) {
  const [open, setOpen] = useState(false);
  const t = it.data.tool;
  const kind = it.kind === "tool" ? (t?.kind ?? "other") : it.kind === "thinking" ? "thinking" : "sub";
  const icon = it.kind === "thinking" ? "sparkle" : it.kind === "text" ? "bot" : toolIcon[kind] ?? "wrench";
  const verb = it.kind === "tool" ? toolVerb[kind] : "";
  const mono = kind === "command";
  return (
    <>
      <div class={`cv-step ${it.status === "failed" ? "failed" : ""}`} onClick={() => setOpen((o) => !o)}>
        {it.status === "running" ? <span class="cv-spin" /> : <Icon name={icon} size={15} />}
        <span class="t">
          {verb && <span class="kind">{verb}</span>}
          {mono ? <code>{stepTitle(it)}</code> : stepTitle(it)}
        </span>
        {it.status === "failed" && <Icon name="alert" size={14} class="cv-bad" />}
      </div>
      {open && it.kind === "thinking" && <div class="cv-thinking">{it.data.text}</div>}
      {open && it.kind === "text" && <div class="cv-thinking">{it.data.text}</div>}
      {open && it.kind === "tool" && (
        <div class="cv-step-detail">
          {t?.input && t.input !== t.title && (
            <>
              <span class="lbl">Input</span>
              <pre>{prettyJSON(t.input)}</pre>
            </>
          )}
          {t?.output ? (
            <>
              <span class="lbl">{t.error ? "Error" : "Output"}</span>
              <pre>{t.output}</pre>
            </>
          ) : (
            <span class="lbl">{it.status === "running" ? "Running…" : "No output"}</span>
          )}
        </div>
      )}
    </>
  );
}

function prettyJSON(s: string): string {
  if (!s.startsWith("{") && !s.startsWith("[")) return s;
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

function Working({ session }: { session: Session }) {
  const a = session.activity;
  const detail = a?.state === "tool" ? a.detail : "";
  const [, tick] = useState(0);
  useEffect(() => {
    const t = setInterval(() => tick((n) => n + 1), 1000);
    return () => clearInterval(t);
  }, []);
  const since = session.conversation?.turn_since ? Math.max(0, Math.round((Date.now() - new Date(session.conversation.turn_since).getTime()) / 1000)) : 0;
  return (
    <div class="cv-working">
      <Avatar id={session.id} size={22} />
      {a?.state === "needs_permission" ? <Icon name="alert" size={15} class="cv-warn" /> : <span class="cv-spin" />}
      <span>{a?.state === "needs_permission" ? "Waiting for you" : detail ? "Working:" : "Working…"}</span>
      {detail && <span class="detail">{detail}</span>}
      {since > 2 && <span>{since >= 60 ? `${Math.floor(since / 60)}m ${since % 60}s` : `${since}s`}</span>}
    </div>
  );
}

function StateBanner({ session }: { session: Session }) {
  const [busy, setBusy] = useState(false);
  const start = async () => {
    setBusy(true);
    try {
      await api.startSession(session.id);
    } finally {
      setBusy(false);
    }
  };
  const label: Record<string, string> = {
    creating: "Starting the agent…",
    stopping: "Stopping…",
    stopped: "This agent is stopped. Messages you send wait until it runs again.",
    failed: `The agent failed${session.state_reason ? ": " + session.state_reason : ""}.`,
    deleting: "Deleting…",
  };
  return (
    <div class={`cv-banner ${session.state === "failed" ? "bad" : ""}`}>
      {session.state === "creating" || session.state === "stopping" ? <span class="cv-spin" /> : <Icon name={session.state === "failed" ? "alert" : "power"} size={17} />}
      <span class="text">{label[session.state] ?? session.state}</span>
      {(session.state === "stopped" || session.state === "failed") && (
        <button class="sh-btn small primary" disabled={busy} onClick={start}>
          <Icon name="play" size={14} /> Start
        </button>
      )}
    </div>
  );
}

function Bottom({ session, queued, working, foreground, onSent }: { session: Session; queued: ConvItem[]; working: boolean; foreground: boolean; onSent: () => void }) {
  const [text, setText] = useState(() => {
    try {
      return localStorage.getItem(draftKey(session.id)) ?? "";
    } catch {
      return "";
    }
  });
  const [files, setFiles] = useState<File[]>([]);
  // Attached files of the unsent message survive too (drafts.ts).
  const filesLoaded = useRef(false);
  useEffect(() => {
    let live = true;
    loadDraft(`session:${session.id}`).then((d) => {
      if (live && d.files.length) setFiles((cur) => (cur.length ? cur : d.files));
      filesLoaded.current = true;
    });
    return () => {
      live = false;
    };
  }, [session.id]);
  useEffect(() => {
    if (filesLoaded.current) saveDraft(`session:${session.id}`, { text: "", files });
  }, [files, session.id]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [editing, setEditing] = useState<{ key: string; text: string } | null>(null);
  const ta = useRef<HTMLTextAreaElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  useEffect(() => {
    try {
      if (text) localStorage.setItem(draftKey(session.id), text);
      else localStorage.removeItem(draftKey(session.id));
    } catch {
      /* ignore */
    }
    const el = ta.current;
    if (el) {
      el.style.height = "auto";
      el.style.height = `${Math.min(el.scrollHeight, window.innerHeight * 0.4)}px`;
    }
  }, [text]);

  const send = async () => {
    if (busy || (!text.trim() && files.length === 0)) return;
    setBusy(true);
    setErr("");
    try {
      await api.sendInput(session.id, text, files);
      setText("");
      setFiles([]);
      onSent();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
      ta.current?.focus();
    }
  };

  const stop = async () => {
    setErr("");
    try {
      await api.interrupt(session.id);
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  const toBackground = async () => {
    setErr("");
    try {
      await api.background(session.id);
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  // Ctrl+B, as in Claude Code's terminal.
  useEffect(() => {
    if (!foreground) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey && e.key.toLowerCase() === "b") {
        e.preventDefault();
        toBackground();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [foreground, session.id]);

  const touch = matchMedia("(pointer: coarse)").matches;
  const empty = !text.trim() && files.length === 0;

  return (
    <div class="cv-bottom">
      <div class="cv-bottom-inner">
        {err && <div class="cv-error" style={{ marginBottom: 8 }}>{err}</div>}
        {queued.length > 0 && (
          <div class="cv-queue">
            <div class="cv-queue-head">
              Queued <span class="muted">· {working ? "sent when the agent is done" : session.state === "running" ? "sending…" : "sent when the agent runs"}</span>
            </div>
            {queued.map((q) => {
              const input = q.data.input ?? "";
              if (editing?.key === q.key)
                return (
                  <div class="cv-queue-item">
                    <textarea rows={2} value={editing.text} onInput={(e) => setEditing({ key: q.key, text: (e.target as HTMLTextAreaElement).value })} />
                    <button
                      class="sh-icon-btn"
                      title="Save"
                      onClick={async () => {
                        try {
                          await api.editInput(session.id, input, editing.text);
                          setEditing(null);
                        } catch (e) {
                          setErr((e as Error).message);
                        }
                      }}
                    >
                      <Icon name="check" size={16} />
                    </button>
                    <button class="sh-icon-btn" title="Cancel edit" onClick={() => setEditing(null)}>
                      <Icon name="x" size={16} />
                    </button>
                  </div>
                );
              return (
                <div class="cv-queue-item">
                  <span class="t">{q.data.text || (q.data.files?.length ? `${q.data.files.length} file(s)` : "")}</span>
                  {working && (
                    <button class="sh-btn small" title="Give it to the agent now, during its turn" onClick={() => api.sendNow(session.id, input).catch((e) => setErr((e as Error).message))}>
                      Send now
                    </button>
                  )}
                  <button class="sh-icon-btn" title="Edit" onClick={() => setEditing({ key: q.key, text: q.data.text ?? "" })}>
                    <Icon name="pencil" size={15} />
                  </button>
                  <button
                    class="sh-icon-btn"
                    title="Remove from the queue"
                    onClick={() => api.cancelInput(session.id, input).catch((e) => setErr((e as Error).message))}
                  >
                    <Icon name="x" size={16} />
                  </button>
                </div>
              );
            })}
          </div>
        )}
        <div class="cv-composer">
          <div style={{ flex: 1, minWidth: 0 }}>
            {files.length > 0 && (
              <div class="cv-attached">
                {files.map((f, i) => (
                  <span class="cv-file">
                    <Icon name="file" size={15} />
                    <span>{f.name}</span>
                    <button class="sh-icon-btn" style={{ width: 22, height: 22 }} onClick={() => setFiles((cur) => cur.filter((_, j) => j !== i))}>
                      <Icon name="x" size={13} />
                    </button>
                  </span>
                ))}
              </div>
            )}
            <div style={{ display: "flex", alignItems: "flex-end", gap: 6 }}>
              <button class="cv-round" title="Attach files" onClick={() => fileInput.current?.click()}>
                <Icon name="plus" size={19} />
              </button>
              <input
                ref={fileInput}
                type="file"
                multiple
                hidden
                onChange={(e) => {
                  const list = Array.from((e.target as HTMLInputElement).files ?? []);
                  setFiles((cur) => [...cur, ...list].slice(0, 5));
                  (e.target as HTMLInputElement).value = "";
                }}
              />
              <textarea
                ref={ta}
                rows={1}
                placeholder={`Message ${session.name}`}
                value={text}
                onInput={(e) => setText((e.target as HTMLTextAreaElement).value)}
                onPaste={(e) => {
                  const pasted = Array.from(e.clipboardData?.files ?? []);
                  if (pasted.length) {
                    e.preventDefault();
                    setFiles((cur) => [...cur, ...pasted].slice(0, 5));
                  }
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && !e.shiftKey && !touch && !e.isComposing) {
                    e.preventDefault();
                    send();
                  }
                }}
              />
              {foreground && (
                <button class="cv-round" title="Move the running command to the background (Ctrl+B)" onClick={toBackground}>
                  <Icon name="toBackground" size={16} />
                </button>
              )}
              {working && empty ? (
                <button class="cv-round send" title="Stop" onClick={stop}>
                  <Icon name="stop" size={16} />
                </button>
              ) : (
                <button class="cv-round send" title={working ? "Queue message" : "Send"} disabled={empty || busy} onClick={send}>
                  <Icon name="arrowUp" size={18} />
                </button>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function ContextRing({ used, size }: { used?: number; size?: number }) {
  if (!used || !size) return null;
  const pct = Math.min(1, used / size);
  const r = 7;
  const c = 2 * Math.PI * r;
  return (
    <span class="sh-chip sh-ctx" title={`Context: ${used.toLocaleString()} of ${size.toLocaleString()} tokens`}>
      <svg width="18" height="18" viewBox="0 0 18 18">
        <circle cx="9" cy="9" r={r} stroke="var(--sh-border)" />
        <circle
          cx="9"
          cy="9"
          r={r}
          stroke={pct > 0.85 ? "var(--sh-danger)" : pct > 0.6 ? "var(--sh-warn)" : "var(--sh-fg)"}
          stroke-dasharray={`${c * pct} ${c}`}
          transform="rotate(-90 9 9)"
          stroke-linecap="round"
        />
      </svg>
      {Math.round(pct * 100)}%
    </span>
  );
}

function SessionMenu({ session }: { session: Session }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) setOpen(false);
    };
    addEventListener("mousedown", close);
    return () => removeEventListener("mousedown", close);
  }, [open]);
  const act = async (fn: () => Promise<unknown>) => {
    setOpen(false);
    try {
      await fn();
    } catch (e) {
      alert((e as Error).message);
    }
  };
  const running = session.state === "running" || session.state === "creating";
  return (
    <div class="sh-menu-wrap" ref={ref}>
      <button class="sh-icon-btn" title="More" onClick={() => setOpen((o) => !o)}>
        <Icon name="more" size={18} />
      </button>
      {open && (
        <div class="sh-menu">
          {running ? (
            <button onClick={() => act(() => api.stopSession(session.id))}>
              <Icon name="power" size={16} /> Stop agent
            </button>
          ) : (
            <button onClick={() => act(() => api.startSession(session.id))}>
              <Icon name="play" size={16} /> Start agent
            </button>
          )}
          <Link href={`/sessions/${session.id}/terminal`}>
            <Icon name="terminal" size={16} /> Terminal
          </Link>
          <Link href={`/sessions/${session.id}/edit`}>
            <Icon name="sliders" size={16} /> Settings
          </Link>
          <hr />
          <button
            class="danger"
            onClick={() =>
              act(async () => {
                if (!confirm(`Delete ${session.name}? Its workspace and conversation are deleted too.`)) return;
                await api.deleteSession(session.id);
                navigate("/");
              })
            }
          >
            <Icon name="x" size={16} /> Delete agent
          </button>
        </div>
      )}
    </div>
  );
}

function InfoPanel({ session, onClose }: { session: Session; onClose: () => void }) {
  const conv = session.conversation ?? {};
  const pct = conv.context && conv.context_window ? Math.min(100, (conv.context / conv.context_window) * 100) : 0;
  return (
    <aside class="sh-panel">
      <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 14 }}>
        <Avatar id={session.id} size={40} status={agentStatus(session)} />
        <div style={{ flex: 1, minWidth: 0 }}>
          <h3 style={{ margin: 0 }}>{session.name}</h3>
          <div style={{ color: "var(--sh-muted)" }}>@{session.handle}</div>
        </div>
        <button class="sh-icon-btn" onClick={onClose} title="Close">
          <Icon name="x" size={16} />
        </button>
      </div>
      <dl>
        <dt>Agent</dt>
        <dd>{agentLabel[session.agent] ?? session.agent}</dd>
        {conv.model && (
          <>
            <dt>Model</dt>
            <dd>{conv.model}</dd>
          </>
        )}
        <dt>State</dt>
        <dd>{session.state}</dd>
        {conv.context_window ? (
          <>
            <dt>Context</dt>
            <dd>
              {(conv.context ?? 0).toLocaleString()} / {conv.context_window.toLocaleString()}
              <div class="sh-meter">
                <span style={{ width: `${pct}%` }} />
              </div>
            </dd>
          </>
        ) : null}
        {(conv.input_tokens || conv.output_tokens) && (
          <>
            <dt>Tokens</dt>
            <dd>
              {(conv.input_tokens ?? 0).toLocaleString()} in · {(conv.output_tokens ?? 0).toLocaleString()} out
            </dd>
          </>
        )}
        {session.repos.length > 0 && (
          <>
            <dt>Repos</dt>
            <dd>{session.repos.map((r) => r.path).join(", ")}</dd>
          </>
        )}
        <dt>Autonomous</dt>
        <dd>{session.autonomous ? "yes" : "no"}</dd>
      </dl>
      <div style={{ marginTop: 16 }}>
        <RoutineList sessionId={session.id} />
      </div>
      <div style={{ marginTop: 16 }}>
        <Memories session={session} />
      </div>
      {session.persona && (
        <>
          <div style={{ marginTop: 14, color: "var(--sh-muted)" }}>Instructions</div>
          <div class="persona">{session.persona}</div>
        </>
      )}
      <div style={{ display: "flex", gap: 8, marginTop: 16, flexWrap: "wrap" }}>
        <Link href={`/sessions/${session.id}/edit`} class="sh-btn small">
          <Icon name="sliders" size={14} /> Settings
        </Link>
        <Link href={`/sessions/${session.id}/terminal`} class="sh-btn small">
          <Icon name="terminal" size={14} /> Terminal
        </Link>
      </div>
    </aside>
  );
}

/** A task list from a tool call (Claude's TodoWrite, Codex and ACP plans). */
type Task = { text: string; status: "pending" | "in_progress" | "completed" };

function taskList(it: ConvItem): Task[] | null {
  const t = it.data.tool;
  if (!t || t.kind !== "task") return null;
  if (t.name === "TodoWrite" && t.input) {
    try {
      const v = JSON.parse(t.input) as { todos?: { content: string; status: Task["status"]; activeForm?: string }[] };
      if (v.todos?.length) return v.todos.map((x) => ({ text: x.status === "in_progress" && x.activeForm ? x.activeForm : x.content, status: x.status }));
    } catch {
      /* not JSON */
    }
  }
  if (t.name === "plan" && t.output) {
    const out: Task[] = [];
    for (const line of t.output.split("\n")) {
      const m = line.match(/^\s*(?:[-*]\s*)?\[( |x|X|~)\]\s*(.+)$/);
      if (m) out.push({ text: m[2], status: m[1] === " " ? "pending" : m[1] === "~" ? "in_progress" : "completed" });
    }
    if (out.length) return out;
    const lines = t.output.split("\n").map((l) => l.trim()).filter(Boolean);
    if (lines.length > 1) return lines.map((l) => ({ text: l.replace(/^[-*\d.)\s]+/, ""), status: "pending" }));
  }
  return null;
}

function TaskCard({ item }: { item: ConvItem }) {
  const tasks = taskList(item) ?? [];
  const done = tasks.filter((t) => t.status === "completed").length;
  const [open, setOpen] = useState(false);
  const all = done === tasks.length;
  return (
    <div class="cv-steps cv-tasks">
      <button class="cv-steps-head" onClick={() => setOpen((o) => !o)}>
        {all ? <Icon name="check" size={16} class="cv-ok" /> : <span class="cv-ring" style={{ ["--p" as string]: `${(done / Math.max(1, tasks.length)) * 100}%` }} />}
        <span class="label">Tasks</span>
        <span class="count">
          {done}/{tasks.length}
        </span>
        <Icon name={open ? "chevronDown" : "chevronRight"} size={15} />
      </button>
      {open && (
        <div class="cv-step-list">
          {tasks.map((t) => (
            <div class={`cv-task ${t.status}`}>
              {t.status === "completed" ? <Icon name="check" size={14} class="cv-ok" /> : t.status === "in_progress" ? <span class="cv-spin" /> : <span class="cv-box" />}
              <span>{t.text}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

/** A permission or a question the agent waits for. */
function RequestCard({ item, session }: { item: ConvItem; session: Session }) {
  const req = item.data.request;
  const [answers, setAnswers] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  if (!req) return null;
  const pending = item.status === "pending";
  const send = async (r: { option?: string; answers?: Record<string, string> }) => {
    setBusy(true);
    setErr("");
    try {
      await api.answerRequest(session.id, req.id, r);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const resp = item.data.response;
  const status = item.status === "cancelled" ? "No longer needed" : resp?.option ? (resp.option === "deny" ? "Denied" : resp.option === "allow_always" ? "Always allowed" : "Allowed") : resp?.answers ? "Answered" : "";
  if (req.kind === "question") {
    const qs = req.questions ?? [];
    return (
      <div class={`cv-request ${pending ? "pending" : ""}`}>
        <div class="cv-request-head">
          <span>{req.title || "A question for you"}</span>
          {pending ? (
            <span class="rt-badge busy">
              <span class="cv-spin" /> Question
            </span>
          ) : (
            <span class="rt-badge">{status}</span>
          )}
        </div>
        {qs.map((q) => {
          const picked = pending ? answers[q.id] : resp?.answers?.[q.id];
          return (
            <div class="cv-question">
              {q.header && <div class="cv-q-header">{q.header}</div>}
              <div class="cv-q-text">{q.question}</div>
              {q.options?.length ? (
                <div class="cv-q-options">
                  {q.options.map((o) => (
                    <button class={picked === o ? "on" : ""} disabled={!pending || busy} onClick={() => setAnswers((a) => ({ ...a, [q.id]: o }))}>
                      {o}
                    </button>
                  ))}
                </div>
              ) : pending ? (
                <input class="rt-input" value={answers[q.id] ?? ""} onInput={(e) => setAnswers((a) => ({ ...a, [q.id]: (e.target as HTMLInputElement).value }))} />
              ) : (
                <div class="cv-q-answer">{picked}</div>
              )}
            </div>
          );
        })}
        {err && <div class="cv-error">{err}</div>}
        {pending && (
          <div class="cv-request-actions">
            <button class="sh-btn small primary" disabled={busy || qs.some((q) => !answers[q.id])} onClick={() => send({ answers })}>
              Answer
            </button>
            <button class="sh-btn small" disabled={busy} onClick={() => send({ answers: {} })}>
              Skip
            </button>
          </div>
        )}
      </div>
    );
  }
  const t = req.tool;
  return (
    <div class={`cv-request ${pending ? "pending" : ""}`}>
      <div class="cv-request-head">
        <span>{req.title || "Allow this?"}</span>
        {pending ? (
          <span class="rt-badge busy">
            <span class="cv-spin" /> Approval
          </span>
        ) : (
          <span class={`rt-badge ${resp?.option === "deny" ? "bad" : resp?.option ? "ok" : ""}`}>{status}</span>
        )}
      </div>
      {t && (t.input || t.title) && <pre class="cv-request-input">{t.kind === "command" ? t.title || t.input : prettyJSON(t.input || t.title || "")}</pre>}
      {t?.output && <div class="rt-hint">{t.output}</div>}
      {err && <div class="cv-error">{err}</div>}
      {pending && (
        <div class="cv-request-actions">
          {(req.options ?? []).filter((o) => o.kind !== "deny").map((o) => (
            <button class={`sh-btn small ${o.kind === "allow" ? "primary" : ""}`} disabled={busy} onClick={() => send({ option: o.id })}>
              {o.label}
            </button>
          ))}
          <span style={{ flex: 1 }} />
          {(req.options ?? []).filter((o) => o.kind === "deny").map((o) => (
            <button class="sh-btn small" disabled={busy} onClick={() => send({ option: o.id })}>
              {o.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/** The model chip: the current model, a menu of the others. */
function ModelPicker({ session }: { session: Session }) {
  const conv = session.conversation ?? {};
  const [open, setOpen] = useState(false);
  const [err, setErr] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) setOpen(false);
    };
    addEventListener("mousedown", close);
    return () => removeEventListener("mousedown", close);
  }, [open]);
  const models = conv.models ?? [];
  const current = models.find((m) => m.id === conv.model);
  const [q, setQ] = useState("");
  const shown = q ? models.filter((m) => `${m.id} ${m.name ?? ""}`.toLowerCase().includes(q.toLowerCase())) : models;
  return (
    <div class="sh-menu-wrap" ref={ref}>
      <button class="sh-chip" title="Model" onClick={() => models.length && setOpen((o) => !o)} disabled={!models.length}>
        <b>{current?.name || conv.model}</b>
        {models.length > 0 && <Icon name="chevronDown" size={13} />}
      </button>
      {open && (
        <div class="sh-menu sh-model-menu">
          {models.length > 8 && (
            <input class="sh-menu-search" placeholder="Search models" value={q} autoFocus onInput={(e) => setQ((e.target as HTMLInputElement).value)} />
          )}
          <div class="sh-menu-scroll">
          {shown.length === 0 && <div class="rt-hint" style={{ padding: 8 }}>No model matches.</div>}
          {shown.map((m) => (
            <button
              onClick={async () => {
                setOpen(false);
                setErr("");
                try {
                  await api.setModel(session.id, m.id);
                } catch (e) {
                  setErr((e as Error).message);
                  alert((e as Error).message);
                }
              }}
            >
              <span style={{ width: 16, flex: "none" }}>{m.id === conv.model && <Icon name="check" size={14} />}</span>
              <span class="sh-model-name">{m.name || m.id}</span>
            </button>
          ))}
          </div>
          {err && <div class="rt-hint" style={{ padding: 8 }}>{err}</div>}
        </div>
      )}
    </div>
  );
}

/** The agent's memories: what it keeps across conversations. */
function Memories({ session }: { session: Session }) {
  const [list, setList] = useState<import("../api").AgentMemory[] | null>(null);
  const [adding, setAdding] = useState(false);
  const [text, setText] = useState("");
  const [editing, setEditing] = useState<{ id: string; text: string } | null>(null);
  const [err, setErr] = useState("");
  const load = () =>
    api
      .memories(session.id)
      .then((r) => setList(r.memories))
      .catch((e) => setErr((e as Error).message));
  useEffect(() => {
    load();
    const t = setInterval(load, 15000);
    return () => clearInterval(t);
  }, [session.id]);
  const act = async (fn: () => Promise<unknown>) => {
    setErr("");
    try {
      await fn();
      await load();
    } catch (e) {
      setErr((e as Error).message);
    }
  };
  return (
    <div class="rt-list">
      <div class="rt-list-head">
        <span>Memories{list?.length ? ` · ${list.length}` : ""}</span>
        <button class="sh-icon-btn" title="Add a memory" onClick={() => setAdding((a) => !a)}>
          <Icon name="plus" size={17} />
        </button>
      </div>
      {err && <div class="cv-error">{err}</div>}
      {adding && (
        <div class="mem-add">
          <textarea class="rt-input" rows={2} value={text} placeholder="Something the agent should always know" onInput={(e) => setText((e.target as HTMLTextAreaElement).value)} />
          <button
            class="sh-btn small primary"
            disabled={!text.trim()}
            onClick={() =>
              act(async () => {
                await api.addMemory(session.id, text);
                setText("");
                setAdding(false);
              })
            }
          >
            Save
          </button>
        </div>
      )}
      {list?.length === 0 && !adding && <div class="rt-hint">None yet. The agent saves what it should remember, and you can add your own.</div>}
      {list?.map((m) =>
        editing?.id === m.id ? (
          <div class="mem-add">
            <textarea class="rt-input" rows={2} value={editing.text} onInput={(e) => setEditing({ id: m.id, text: (e.target as HTMLTextAreaElement).value })} />
            <button
              class="sh-btn small primary"
              onClick={() =>
                act(async () => {
                  await api.updateMemory(session.id, m.id, editing.text);
                  setEditing(null);
                })
              }
            >
              Save
            </button>
          </div>
        ) : (
          <div class="mem-row">
            <Icon name={m.source === "owner" ? "user" : "sparkle"} size={14} />
            <span class="t">{m.text}</span>
            <button class="sh-icon-btn" title="Edit" onClick={() => setEditing({ id: m.id, text: m.text })}>
              <Icon name="pencil" size={13} />
            </button>
            <button class="sh-icon-btn" title="Forget" onClick={() => act(() => api.deleteMemory(session.id, m.id))}>
              <Icon name="x" size={14} />
            </button>
          </div>
        ),
      )}
    </div>
  );
}

/** What the agent left running in the background, until it reports back. */
function BackgroundChip({ items }: { items: { id: string; title: string }[] }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) setOpen(false);
    };
    addEventListener("mousedown", close);
    return () => removeEventListener("mousedown", close);
  }, [open]);
  return (
    <div class="sh-menu-wrap" ref={ref}>
      <button class="sh-chip sh-bg-chip" title="Running in the background" onClick={() => setOpen((o) => !o)}>
        <span class="cv-spin" />
        {items.length} in background
      </button>
      {open && (
        <div class="sh-menu">
          <div class="rt-hint" style={{ padding: "4px 10px 6px" }}>
            The agent comes back to these when they finish.
          </div>
          {items.map((b) => (
            <div class="sh-bg-row">
              <span class="cv-spin" />
              <code>{b.title}</code>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
