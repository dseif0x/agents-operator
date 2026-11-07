// An agent's own conversation: your messages, its answers, its work steps
// folded per turn, the queue, and the composer.
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "preact/hooks";
import { createPortal } from "preact/compat";
import { api, subscribeItems, type ConvItem, type Session } from "../api";
import { Link, navigate } from "../router";
import { copyCode, renderMessage } from "../markdown";
import { Avatar } from "./Avatar";
import { Icon } from "./icons";
import { agentLabel, agentStatus, longTime } from "./format";

type Block =
  | { type: "user"; item: ConvItem }
  | { type: "text"; item: ConvItem }
  | { type: "steps"; items: ConvItem[]; key: string }
  | { type: "error"; item: ConvItem }
  | { type: "notice"; item: ConvItem }
  | { type: "date"; key: string; at: string };

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
  const status = agentStatus(session);

  const header = (
    <>
      <span class="sh-title">
        <Avatar id={id} size={22} status={status} />
        <span>{session.name}</span>
      </span>
      {conv.model && (
        <span class="sh-chip" title="Model">
          <b>{conv.model}</b>
        </span>
      )}
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
              <BlockView key={b.type === "steps" || b.type === "date" ? b.key : b.item.key} b={b} session={session} />
            ))}
            {working && <Working session={session} />}
          </div>
        </div>
        <Bottom session={session} queued={queued} working={working} onSent={() => (stick.current = true)} />
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
  for (const it of items) {
    const at = new Date(it.created_at);
    const day = at.toDateString();
    if (day !== lastDay || at.getTime() - lastAt > 3 * 3600_000) {
      out.push({ type: "date", key: `date-${it.id}`, at: it.created_at });
      lastDay = day;
      steps = null;
    }
    lastAt = at.getTime();
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
      <span class="cv-spin" />
      <span>{detail ? "Working:" : "Working…"}</span>
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

function Bottom({ session, queued, working, onSent }: { session: Session; queued: ConvItem[]; working: boolean; onSent: () => void }) {
  const [text, setText] = useState(() => {
    try {
      return localStorage.getItem(draftKey(session.id)) ?? "";
    } catch {
      return "";
    }
  });
  const [files, setFiles] = useState<File[]>([]);
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
