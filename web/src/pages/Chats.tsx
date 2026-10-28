import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { api, subscribeChats, subscribeSessions, type Chat, type ChatMember, type ChatMessage, type Session, type User } from "../api";
import { Nav } from "../components/Nav";
import { PushPrompt } from "../components/Notifications";
import { Link, navigate } from "../router";
import { copyCode, mentionedHandles, renderMessage } from "../markdown";

// Chats: groups of the owner's agent sessions, like a group chat. The list
// is on the left (the whole screen on a phone until a chat is open), the
// open chat on the right.

/** A stable colour per session for avatars and names. */
function hue(id: string): number {
  let h = 0;
  for (const c of id) h = (h * 31 + c.charCodeAt(0)) % 360;
  return h;
}

function initials(name: string): string {
  const parts = name.split(/[\s._-]+/).filter(Boolean);
  return ((parts[0]?.[0] ?? "?") + (parts[1]?.[0] ?? "")).toUpperCase();
}

function Avatar(props: { id: string; name: string; owner?: boolean }) {
  if (props.owner) return <span class="chat-avatar owner">you</span>;
  return (
    <span class="chat-avatar" style={`background:hsl(${hue(props.id)} 55% 42%)`}>
      {initials(props.name)}
    </span>
  );
}

/** Images (by the type the hub sniffed) are shown inline; other files are downloads. */
const imageTypes = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
const isImage = (a: { content_type: string }) => imageTypes.has(a.content_type);

function sizeLabel(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10 * 1024 ? 1 : 0)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function timeLabel(iso: string): string {
  const d = new Date(iso);
  const now = new Date();
  const hm = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  if (d.toDateString() === now.toDateString()) return hm;
  return d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + hm;
}

function preview(m?: ChatMessage): string {
  if (!m) return "No messages yet";
  const who = m.author.kind === "owner" ? "You" : m.author.name;
  const text = m.text.replace(/\s+/g, " ").trim() || (m.attachments.length ? (m.attachments.every(isImage) ? "🖼 image" : "📎 file") : "");
  return `${who}: ${text}`;
}

const busyActivities = new Set(["thinking", "tool"]);

export function Chats(props: { user: User; onLogout: () => void; chatId?: string }) {
  const [chats, setChats] = useState<Chat[] | null>(null);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [error, setError] = useState("");
  const [creating, setCreating] = useState(false);
  // Every message event goes to the open chat's listener: one state value
  // would drop all but the last of several events arriving together.
  const listeners = useRef(new Set<(m: ChatMessage) => void>());
  // Bumped when the event stream reconnects, so the open chat catches up.
  const [epoch, setEpoch] = useState(0);
  const openId = useRef(props.chatId);
  openId.current = props.chatId;

  const load = () =>
    Promise.all([api.chats(), api.sessions()])
      .then(([c, s]) => {
        setChats(c.chats);
        setSessions(s.sessions);
      })
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    load();
    return subscribeChats(
      (id, chat) =>
        setChats((cur) => {
          if (!cur) return cur;
          if (!chat) return cur.filter((c) => c.id !== id);
          return cur.some((c) => c.id === id) ? cur.map((c) => (c.id === id ? chat : c)) : [...cur, chat];
        }),
      (msg) => {
        listeners.current.forEach((fn) => fn(msg));
        setChats((cur) =>
          cur && cur.map((c) => {
            if (c.id !== msg.chat_id) return c;
            const isNew = !c.last_message || msg.id > c.last_message.id;
            const unread = isNew && msg.author.kind === "session" && openId.current !== c.id ? c.unread + 1 : c.unread;
            return { ...c, last_message: isNew ? msg : c.last_message, unread };
          }),
        );
      },
      // Reconnected: catch up on what was missed.
      () => {
        load();
        setEpoch((e) => e + 1);
      },
    );
  }, []);

  // Members' state and activity ("is working…") follow their sessions.
  useEffect(
    () =>
      subscribeSessions((type, id, sess) => {
        if (type !== "session" || !sess) return;
        setChats(
          (cur) =>
            cur &&
            cur.map((c) =>
              c.members.some((m) => m.session_id === id)
                ? {
                    ...c,
                    members: c.members.map((m) =>
                      m.session_id === id ? { ...m, name: sess.name, state: sess.state, activity: sess.state === "running" ? sess.activity?.state : undefined } : m,
                    ),
                  }
                : c,
            ),
        );
      }),
    [],
  );

  // Picking a chat leaves the new-chat form.
  useEffect(() => setCreating(false), [props.chatId]);

  const current = chats?.find((c) => c.id === props.chatId);
  const sorted = useMemo(
    () => [...(chats ?? [])].sort((a, b) => (b.last_message?.created_at ?? b.created_at).localeCompare(a.last_message?.created_at ?? a.created_at)),
    [chats],
  );

  return (
    <div class="page chats-page">
      <Nav user={props.user} onLogout={props.onLogout} />
      {error && <div class="error">{error}</div>}
      {/* On a phone the layout shows either the list or the main pane
          (has-open); the new-chat form is in the main pane, so it counts. */}
      <div class={`chats-layout ${props.chatId || creating ? "has-open" : ""}`}>
        <aside class="chat-list">
          <div class="chat-list-head">
            <b>Chats</b>
            <span style="flex:1" />
            <button class="btn small primary" onClick={() => setCreating(true)}>
              New chat
            </button>
          </div>
          <PushPrompt />
          {chats === null && <div class="muted pad">Loading…</div>}
          {chats?.length === 0 && !creating && (
            <div class="muted pad">No chats yet. Create one and add agent sessions; they talk to you and to each other there.</div>
          )}
          {sorted.map((c) => {
            const needsYou = c.members.some((m) => m.activity === "needs_permission");
            return (
              <Link href={`/chats/${c.id}`} class={`chat-item ${c.id === props.chatId ? "active" : ""}`}>
                <div class="chat-item-top">
                  <span class="chat-item-name">{c.name}</span>
                  {c.muted && <span class="muted" title="Muted">🔕</span>}
                  {needsYou && <span class="badge attention">needs you</span>}
                  <span style="flex:1" />
                  {c.last_message && <span class="muted chat-item-time">{timeLabel(c.last_message.created_at)}</span>}
                </div>
                <div class="chat-item-bottom">
                  <span class="muted chat-item-preview">{preview(c.last_message)}</span>
                  {c.unread > 0 && <span class="unread">{c.unread}</span>}
                </div>
              </Link>
            );
          })}
        </aside>
        <main class="chat-main">
          {creating ? (
            <NewChat
              sessions={sessions}
              onCancel={() => setCreating(false)}
              onCreated={(c) => {
                setCreating(false);
                setChats((cur) => (cur?.some((x) => x.id === c.id) ? cur : [...(cur ?? []), c]));
                navigate(`/chats/${c.id}`);
              }}
            />
          ) : current ? (
            <ChatView
              key={current.id}
              chat={current}
              sessions={sessions}
              subscribe={(fn) => {
                listeners.current.add(fn);
                return () => listeners.current.delete(fn);
              }}
              epoch={epoch}
              onChanged={(c) => setChats((cur) => cur && cur.map((x) => (x.id === c.id ? c : x)))}
              onRead={() => setChats((cur) => cur && cur.map((x) => (x.id === current.id ? { ...x, unread: 0 } : x)))}
            />
          ) : (
            <div class="chat-empty muted">{props.chatId && chats ? "This chat does not exist (any more)." : "Pick a chat, or create one."}</div>
          )}
        </main>
      </div>
    </div>
  );
}

function NewChat(props: { sessions: Session[]; onCancel: () => void; onCreated: (c: Chat) => void }) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [members, setMembers] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const agents = props.sessions.filter((s) => s.agent !== "shell");
  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      props.onCreated(await api.createChat(name, members, description));
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form class="card new-chat" onSubmit={submit}>
      <h3 style="margin:0">New chat</h3>
      <label>Name</label>
      <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} placeholder="launch, login-bug, research…" required autoFocus />
      <label>Description (optional)</label>
      <textarea
        value={description}
        onInput={(e) => setDescription((e.target as HTMLTextAreaElement).value)}
        rows={3}
        placeholder="The goal, and who does what. Agents can look it up when they need it."
      />
      <label>Agents</label>
      {agents.length === 0 && <div class="muted">No agent sessions yet.</div>}
      <div class="member-picks">
        {agents.map((s) => (
          <label class="member-pick">
            <input
              type="checkbox"
              checked={members.includes(s.id)}
              onChange={(e) => {
                const on = (e.target as HTMLInputElement).checked;
                setMembers((cur) => (on ? [...cur, s.id] : cur.filter((x) => x !== s.id)));
              }}
            />
            <Avatar id={s.id} name={s.name} />
            <span>
              {s.name} <span class="muted">@{s.handle} · {s.agent}</span>
            </span>
          </label>
        ))}
      </div>
      {error && <div class="error">{error}</div>}
      <div class="row" style="margin-top:12px">
        <button class="btn primary" disabled={busy || !name.trim()}>
          Create
        </button>
        <button type="button" class="btn" onClick={props.onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

const PAGE = 50;

function ChatView(props: {
  chat: Chat;
  sessions: Session[];
  subscribe: (fn: (m: ChatMessage) => void) => () => void;
  epoch: number;
  onChanged: (c: Chat) => void;
  onRead: () => void;
}) {
  const { chat } = props;
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [more, setMore] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [info, setInfo] = useState(false);
  const [replyTo, setReplyTo] = useState<ChatMessage | null>(null);
  const [lightbox, setLightbox] = useState<string | null>(null);
  const scroller = useRef<HTMLDivElement>(null);
  const atBottom = useRef(true);

  const members = chat.members;
  const memberById = useMemo(() => new Map(members.map((m) => [m.session_id, m])), [members]);

  const scrollDown = () =>
    requestAnimationFrame(() => {
      const el = scroller.current;
      if (el) el.scrollTop = el.scrollHeight;
    });

  useEffect(() => {
    setLoading(true);
    api
      .chatMessages(chat.id, { limit: PAGE })
      .then((r) => {
        // Merge rather than replace: events may have arrived meanwhile.
        merge(r.messages);
        setMore(r.messages.length === PAGE);
        scrollDown();
      })
      .catch((e) => setError((e as Error).message))
      .finally(() => setLoading(false));
  }, [chat.id]);

  const merge = (list: ChatMessage[]) => {
    if (!list.length) return;
    setMessages((cur) => {
      const byId = new Map(cur.map((x) => [x.id, x]));
      for (const m of list) byId.set(m.id, m);
      return [...byId.values()].sort((a, b) => a.id - b.id);
    });
    if (atBottom.current) scrollDown();
  };

  // New messages and delivery updates from the event stream.
  useEffect(() => props.subscribe((m) => m.chat_id === chat.id && merge([m])), [chat.id]);

  // Catch up after the stream reconnected or the tab comes back: fetch what
  // came after the newest message on screen (and refresh the last ones'
  // delivery state).
  const newest = useRef(0);
  newest.current = messages.length ? messages[messages.length - 1].id : 0;
  const catchUp = () => {
    const after = Math.max(0, newest.current - 20);
    if (!newest.current) return;
    api
      .chatMessages(chat.id, { after, limit: 200 })
      .then((r) => merge(r.messages))
      .catch(() => undefined);
  };
  useEffect(() => {
    if (props.epoch) catchUp();
  }, [props.epoch]);
  useEffect(() => {
    const onVisible = () => document.visibilityState === "visible" && catchUp();
    document.addEventListener("visibilitychange", onVisible);
    // A safety net for an event stream a proxy holds back: look for new
    // messages now and then while the chat is on screen.
    const timer = setInterval(() => document.visibilityState === "visible" && catchUp(), 20000);
    return () => {
      document.removeEventListener("visibilitychange", onVisible);
      clearInterval(timer);
    };
  }, [chat.id]);

  // Reading what is on screen.
  const lastId = messages.length ? messages[messages.length - 1].id : 0;
  useEffect(() => {
    if (!lastId || document.hidden) return;
    api.markChatRead(chat.id, lastId).catch(() => undefined);
    props.onRead();
  }, [lastId]);

  const loadOlder = async () => {
    if (!messages.length) return;
    const el = scroller.current;
    const before = el ? el.scrollHeight - el.scrollTop : 0;
    const r = await api.chatMessages(chat.id, { before: messages[0].id, limit: PAGE });
    setMessages((cur) => [...r.messages, ...cur]);
    setMore(r.messages.length === PAGE);
    requestAnimationFrame(() => {
      if (el) el.scrollTop = el.scrollHeight - before;
    });
  };

  const byId = useMemo(() => new Map(messages.map((m) => [m.id, m])), [messages]);
  const working = members.filter((m) => m.state === "running" && m.activity && busyActivities.has(m.activity));

  const onBodyClick = (e: MouseEvent) => {
    const copy = (e.target as HTMLElement).closest<HTMLElement>(".code-copy");
    if (copy) {
      e.preventDefault();
      copyCode(copy);
      return;
    }
    const a = (e.target as HTMLElement).closest("a");
    const href = a?.getAttribute("href") || "";
    if (href.startsWith("#mention-")) {
      e.preventDefault();
      const sid = href.slice("#mention-".length);
      if (sid !== "everyone") navigate(`/sessions/${sid}`);
    }
  };

  return (
    <div class="chat-view">
      <header class="chat-head">
        <Link href="/chats" class="btn small chat-back" title="All chats">
          ←
        </Link>
        <div class="chat-head-title" onClick={() => setInfo(!info)}>
          <b>{chat.name}</b>
          <span class="muted chat-head-members">
            {members.length ? members.map((m) => m.name).join(", ") : "no agents yet"}
            {chat.muted ? " · muted" : ""}
          </span>
        </div>
        <button class="btn small" onClick={() => setInfo(!info)} title="Chat info">
          ⓘ
        </button>
      </header>
      <div class="chat-body-wrap">
        <div
          class="chat-body"
          ref={scroller}
          onScroll={(e) => {
            const el = e.currentTarget as HTMLDivElement;
            atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80;
          }}
          onClick={onBodyClick}
        >
          {more && (
            <button class="btn small load-older" onClick={loadOlder}>
              Load older messages
            </button>
          )}
          {loading && <div class="muted pad">Loading…</div>}
          {error && <div class="error">{error}</div>}
          {!loading && messages.length === 0 && (
            <div class="muted pad">
              No messages yet. A message without mentions goes to every agent here, and each decides whether to answer;{" "}
              <code>@name</code> or a reply addresses one; <code>@everyone</code> asks all of them to answer.
            </div>
          )}
          {messages.map((m, i) => {
            const prev = messages[i - 1];
            const grouped =
              prev &&
              prev.author.kind === m.author.kind &&
              prev.author.session_id === m.author.session_id &&
              new Date(m.created_at).getTime() - new Date(prev.created_at).getTime() < 5 * 60 * 1000 &&
              !m.reply_to;
            return (
              <Message
                m={m}
                grouped={!!grouped}
                parent={m.reply_to ? byId.get(m.reply_to) : undefined}
                members={members}
                memberById={memberById}
                onReply={() => setReplyTo(m)}
                onImage={setLightbox}
              />
            );
          })}
        </div>
        {info && <ChatInfo chat={chat} sessions={props.sessions} onChanged={props.onChanged} onClose={() => setInfo(false)} />}
      </div>
      {working.length > 0 && (
        <div class="chat-working muted">
          {working.map((m) => m.name).join(", ")} {working.length === 1 ? "is" : "are"} working…
        </div>
      )}
      <Composer
        chat={chat}
        replyTo={replyTo}
        onCancelReply={() => setReplyTo(null)}
        onSent={(m) => {
          setReplyTo(null);
          setMessages((cur) => (cur.some((x) => x.id === m.id) ? cur : [...cur, m].sort((a, b) => a.id - b.id)));
          scrollDown();
        }}
      />
      {lightbox && (
        <div class="lightbox" onClick={() => setLightbox(null)}>
          <img src={lightbox} alt="" />
        </div>
      )}
    </div>
  );
}

function Message(props: {
  m: ChatMessage;
  grouped: boolean;
  parent?: ChatMessage;
  members: ChatMember[];
  memberById: Map<string, ChatMember>;
  onReply: () => void;
  onImage: (url: string) => void;
}) {
  const { m } = props;
  const owner = m.author.kind === "owner";
  const html = useMemo(() => renderMessage(m.text, props.members), [m.text, props.members]);
  const queued = Object.entries(m.deliveries)
    .filter(([, st]) => st === "queued")
    .map(([sid]) => props.memberById.get(sid)?.name ?? "a removed session");
  const color = owner ? undefined : `color:hsl(${hue(m.author.session_id || "")} 60% 60%)`;
  return (
    <div class={`chat-msg ${owner ? "mine" : ""} ${props.grouped ? "grouped" : ""}`} id={`msg-${m.id}`}>
      <div class="chat-msg-avatar">{!props.grouped && <Avatar id={m.author.session_id || "owner"} name={m.author.name} owner={owner} />}</div>
      <div class="chat-msg-main">
        {!props.grouped && (
          <div class="chat-msg-head">
            {owner ? (
              <b>You</b>
            ) : m.author.session_id && !m.author.deleted ? (
              <Link href={`/sessions/${m.author.session_id}`} class="chat-author">
                <b style={color}>{m.author.name}</b>
              </Link>
            ) : (
              <b style={color}>{m.author.name} (deleted)</b>
            )}
            {m.author.handle && <span class="muted chat-handle">@{m.author.handle}</span>}
            <span class="muted chat-time">{timeLabel(m.created_at)}</span>
          </div>
        )}
        {m.reply_to && (
          <a
            class="chat-quote"
            href={`#msg-${m.reply_to}`}
            onClick={(e) => {
              e.preventDefault();
              document.getElementById(`msg-${m.reply_to}`)?.scrollIntoView({ behavior: "smooth", block: "center" });
            }}
          >
            ↪ {props.parent ? `${props.parent.author.kind === "owner" ? "You" : props.parent.author.name}: ${props.parent.text.slice(0, 140)}` : `message #${m.reply_to}`}
          </a>
        )}
        {m.text && <div class="chat-text md" dangerouslySetInnerHTML={{ __html: html }} />}
        {m.attachments.some(isImage) && (
          <div class="chat-images">
            {m.attachments.filter(isImage).map((a) => (
              <img src={a.url} alt={a.name} loading="lazy" onClick={() => props.onImage(a.url)} />
            ))}
          </div>
        )}
        {m.attachments.some((a) => !isImage(a)) && (
          <div class="chat-files">
            {m.attachments
              .filter((a) => !isImage(a))
              .map((a) => (
                <a class="chat-file" href={a.url} download={a.name} title={`Download ${a.name}`}>
                  <span class="chat-file-icon">📎</span>
                  <span class="chat-file-name">{a.name}</span>
                  <span class="muted chat-file-size">{sizeLabel(a.size)}</span>
                </a>
              ))}
          </div>
        )}
        {queued.length > 0 && <div class="muted chat-queued">queued for {queued.join(", ")}</div>}
      </div>
      <button class="btn small chat-reply" onClick={props.onReply} title="Reply">
        ↩
      </button>
    </div>
  );
}

function Composer(props: { chat: Chat; replyTo: ChatMessage | null; onCancelReply: () => void; onSent: (m: ChatMessage) => void }) {
  const { chat } = props;
  const [text, setText] = useState("");
  const [files, setFiles] = useState<File[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [suggest, setSuggest] = useState<{ query: string; start: number; index: number } | null>(null);
  const ta = useRef<HTMLTextAreaElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const previews = useMemo(() => files.map((f) => (f.type.startsWith("image/") ? URL.createObjectURL(f) : "")), [files]);
  useEffect(() => () => previews.forEach((u) => u && URL.revokeObjectURL(u)), [previews]);

  useEffect(() => {
    if (props.replyTo) ta.current?.focus();
  }, [props.replyTo]);

  // Grow with the text, up to a point.
  useEffect(() => {
    const el = ta.current;
    if (!el) return;
    el.style.height = "auto";
    el.style.height = Math.min(el.scrollHeight, 220) + "px";
  }, [text]);

  const options = useMemo(() => {
    if (!suggest) return [];
    const q = suggest.query.toLowerCase();
    const list: { handle: string; label: string; sub: string }[] = chat.members
      .filter((m) => m.name.toLowerCase().includes(q) || m.handle.includes(q))
      .map((m) => ({ handle: m.handle, label: m.name, sub: "@" + m.handle }));
    if ("everyone".startsWith(q)) list.push({ handle: "everyone", label: "everyone", sub: "every agent answers" });
    return list.slice(0, 8);
  }, [suggest, chat.members]);

  const updateSuggest = (value: string, caret: number) => {
    const m = /(^|[\s(])@([\w.-]*)$/.exec(value.slice(0, caret));
    setSuggest(m ? { query: m[2], start: caret - m[2].length - 1, index: 0 } : null);
  };

  const pick = (handle: string) => {
    if (!suggest || !ta.current) return;
    const caret = ta.current.selectionStart;
    const next = text.slice(0, suggest.start) + "@" + handle + " " + text.slice(caret);
    setText(next);
    setSuggest(null);
    const pos = suggest.start + handle.length + 2;
    requestAnimationFrame(() => {
      ta.current?.focus();
      ta.current?.setSelectionRange(pos, pos);
    });
  };

  // Who the message goes to, as the hub will work it out.
  const target = useMemo(() => {
    const handles = mentionedHandles(text);
    if (handles.includes("everyone")) return "to everyone: every agent is asked to answer";
    const names = handles.map((h) => chat.members.find((m) => m.handle === h)?.name).filter(Boolean) as string[];
    const replyAuthor = props.replyTo?.author.kind === "session" ? props.replyTo.author.name : undefined;
    if (replyAuthor && !names.includes(replyAuthor)) names.push(replyAuthor);
    if (names.length) return "to " + names.join(", ");
    return chat.members.length ? "to everyone: whoever has something to say answers" : "no agents in this chat yet";
  }, [text, chat.members, props.replyTo]);

  const addFiles = (list: FileList | File[] | null) => {
    const picked = Array.from(list ?? []);
    const big = picked.find((f) => f.size > 10 * 1024 * 1024);
    if (big) setError(`${big.name} is larger than 10 MB`);
    const ok = picked.filter((f) => f.size > 0 && f.size <= 10 * 1024 * 1024);
    if (ok.length) setFiles((cur) => [...cur, ...ok].slice(0, 5));
  };

  const send = async () => {
    if (busy || (!text.trim() && !files.length)) return;
    setBusy(true);
    setError("");
    try {
      const m = await api.postChatMessage(chat.id, text, props.replyTo?.id ?? 0, files);
      setText("");
      setFiles([]);
      props.onSent(m);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const onKeyDown = (e: KeyboardEvent) => {
    if (suggest && options.length) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        e.preventDefault();
        const d = e.key === "ArrowDown" ? 1 : -1;
        setSuggest({ ...suggest, index: (suggest.index + d + options.length) % options.length });
        return;
      }
      if (e.key === "Enter" || e.key === "Tab") {
        e.preventDefault();
        pick(options[suggest.index].handle);
        return;
      }
      if (e.key === "Escape") {
        setSuggest(null);
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !matchMedia("(pointer: coarse)").matches) {
      e.preventDefault();
      send();
    }
  };

  return (
    <div
      class="composer"
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault();
        addFiles(e.dataTransfer?.files ?? null);
      }}
    >
      {props.replyTo && (
        <div class="composer-reply">
          <span class="muted">
            ↪ Replying to {props.replyTo.author.kind === "owner" ? "yourself" : props.replyTo.author.name}: {props.replyTo.text.slice(0, 100)}
          </span>
          <button class="btn small" onClick={props.onCancelReply} title="Cancel reply">
            ✕
          </button>
        </div>
      )}
      {files.length > 0 && (
        <div class="composer-files">
          {files.map((f, i) => (
            <span class="composer-file">
              {previews[i] ? (
                <img src={previews[i]} alt={f.name} />
              ) : (
                <span class="composer-file-chip" title={f.name}>
                  📎 {f.name} <span class="muted">{sizeLabel(f.size)}</span>
                </span>
              )}
              <button class="btn small" onClick={() => setFiles((cur) => cur.filter((_, j) => j !== i))} title="Remove">
                ✕
              </button>
            </span>
          ))}
        </div>
      )}
      {suggest && options.length > 0 && (
        <div class="mention-menu">
          {options.map((o, i) => (
            <div
              class={`mention-option ${i === suggest.index ? "active" : ""}`}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(o.handle);
              }}
            >
              <b>{o.label}</b> <span class="muted">{o.sub}</span>
            </div>
          ))}
        </div>
      )}
      <div class="composer-row">
        <button class="btn small" onClick={() => fileInput.current?.click()} title="Attach files (images are shown inline)">
          📎
        </button>
        <input
          ref={fileInput}
          type="file"
          multiple
          hidden
          onChange={(e) => {
            addFiles((e.target as HTMLInputElement).files);
            (e.target as HTMLInputElement).value = "";
          }}
        />
        <textarea
          ref={ta}
          rows={1}
          value={text}
          placeholder={`Message ${chat.name}`}
          onInput={(e) => {
            const el = e.target as HTMLTextAreaElement;
            setText(el.value);
            updateSuggest(el.value, el.selectionStart);
          }}
          onClick={(e) => {
            const el = e.target as HTMLTextAreaElement;
            updateSuggest(el.value, el.selectionStart);
          }}
          onBlur={() => setTimeout(() => setSuggest(null), 150)}
          onKeyDown={onKeyDown}
          onPaste={(e) => {
            const pasted = Array.from(e.clipboardData?.files ?? []);
            if (pasted.length) {
              e.preventDefault();
              addFiles(pasted);
            }
          }}
        />
        <button class="btn primary" disabled={busy || (!text.trim() && !files.length)} onClick={send}>
          Send
        </button>
      </div>
      <div class="muted composer-hint">
        {target}
        {error && <span class="error-text"> · {error}</span>}
      </div>
    </div>
  );
}

function ChatInfo(props: { chat: Chat; sessions: Session[]; onChanged: (c: Chat) => void; onClose: () => void }) {
  const { chat } = props;
  const [name, setName] = useState(chat.name);
  const [description, setDescription] = useState(chat.description);
  const [add, setAdd] = useState("");
  const [error, setError] = useState("");
  const [open, setOpen] = useState<string | null>(null);
  useEffect(() => {
    setName(chat.name);
    setDescription(chat.description);
  }, [chat.id]);

  const run = async (f: () => Promise<Chat>) => {
    setError("");
    try {
      props.onChanged(await f());
    } catch (err) {
      setError((err as Error).message);
    }
  };
  const candidates = props.sessions.filter((s) => s.agent !== "shell" && !chat.members.some((m) => m.session_id === s.id));
  const dirty = name !== chat.name || description !== chat.description;

  return (
    <aside class="chat-info">
      <div class="row" style="align-items:center">
        <b style="flex:1">Chat info</b>
        <button class="btn small" onClick={props.onClose}>
          ✕
        </button>
      </div>
      <label>Name</label>
      <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
      <label>Description</label>
      <textarea
        rows={4}
        value={description}
        onInput={(e) => setDescription((e.target as HTMLTextAreaElement).value)}
        placeholder="The goal, and who does what. Agents read it with chat_info."
      />
      {dirty && (
        <button class="btn small primary" onClick={() => run(() => api.updateChat(chat.id, { name, description }))}>
          Save
        </button>
      )}
      <label>Agents</label>
      {chat.members.length === 0 && <div class="muted">None yet.</div>}
      {chat.members.map((m) => (
        <div class="member-row">
          <div class="row" style="align-items:center;gap:8px">
            <Avatar id={m.session_id} name={m.name} />
            <div style="flex:1;min-width:0">
              <Link href={`/sessions/${m.session_id}`}>{m.name}</Link>
              <div class="muted" style="font-size:12px">
                @{m.handle} · {m.agent} · {m.state}
                {m.activity ? ` · ${m.activity.replace("_", " ")}` : ""}
                {m.muted ? " · muted" : ""}
              </div>
            </div>
          </div>
          <div class="row member-actions">
            {m.persona && (
              <button class="btn small" onClick={() => setOpen(open === m.session_id ? null : m.session_id)}>
                Persona
              </button>
            )}
            <button class="btn small" onClick={() => run(() => api.muteChatMember(chat.id, m.session_id, !m.muted))}>
              {m.muted ? "Unmute" : "Mute"}
            </button>
            <button
              class="btn small danger"
              onClick={() => confirm(`Remove ${m.name} from ${chat.name}?`) && run(() => api.removeChatMember(chat.id, m.session_id))}
            >
              Remove
            </button>
          </div>
          {open === m.session_id && <div class="member-persona">{m.persona}</div>}
        </div>
      ))}
      {candidates.length > 0 && (
        <div class="row" style="margin-top:8px;gap:6px">
          <select value={add} onChange={(e) => setAdd((e.target as HTMLSelectElement).value)} style="flex:1">
            <option value="">Add an agent…</option>
            {candidates.map((s) => (
              <option value={s.id}>
                {s.name} (@{s.handle})
              </option>
            ))}
          </select>
          <button
            class="btn small"
            disabled={!add}
            onClick={() =>
              run(async () => {
                const c = await api.addChatMember(chat.id, add);
                setAdd("");
                return c;
              })
            }
          >
            Add
          </button>
        </div>
      )}
      <div class="row" style="margin-top:16px;gap:6px;flex-wrap:wrap">
        <button class="btn small" onClick={() => run(() => api.updateChat(chat.id, { muted: !chat.muted }))}>
          {chat.muted ? "Unmute chat" : "Mute chat"}
        </button>
        <button
          class="btn small danger"
          onClick={async () => {
            if (!confirm(`Delete ${chat.name} with all its messages and images?`)) return;
            try {
              await api.deleteChat(chat.id);
              navigate("/chats", true);
            } catch (err) {
              setError((err as Error).message);
            }
          }}
        >
          Delete chat
        </button>
      </div>
      <p class="muted" style="font-size:12px">
        Muting a chat stops deliveries to its agents; a muted agent gets nothing and its messages reach no other agent.
      </p>
      {error && <div class="error">{error}</div>}
    </aside>
  );
}
