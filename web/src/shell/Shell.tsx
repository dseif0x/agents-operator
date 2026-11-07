// The workspace, laid out like OpenBot: an icon rail, a sidebar of agents
// and chats, and the open conversation with its tabs.
import { useEffect, useMemo, useState } from "preact/hooks";
import { api, subscribeChats, subscribeSessions, type Chat, type Session, type User } from "../api";
import { Link, navigate, type Route } from "../router";
import { applyTheme, currentTheme } from "../theme";
import { Chats } from "../pages/Chats";
import { Avatar } from "./Avatar";
import { Icon } from "./icons";
import { AgentChat } from "./AgentChat";
import { RoutinesPage } from "./Routines";
import { agentLine, agentStatus, lastActive, needsYou, roleTag, shortTime } from "./format";
import "./shell.css";

const TABS_KEY = "agents-operator.tabs";

function loadTabs(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(TABS_KEY) || "[]");
    return Array.isArray(v) ? v.filter((x) => typeof x === "string").slice(0, 12) : [];
  } catch {
    return [];
  }
}

function saveTabs(tabs: string[]) {
  try {
    localStorage.setItem(TABS_KEY, JSON.stringify(tabs));
  } catch {
    /* private mode */
  }
}

export function Shell(props: { user: User; route: Route; onLogout: () => void }) {
  const { route } = props;
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [chats, setChats] = useState<Chat[] | null>(null);
  const [error, setError] = useState("");
  const [search, setSearch] = useState("");
  const [tabs, setTabs] = useState<string[]>(loadTabs);
  const [theme, setTheme] = useState(currentTheme());
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});

  const load = () => {
    api
      .sessions()
      .then((r) => setSessions(r.sessions))
      .catch((e) => setError((e as Error).message));
    api
      .chats()
      .then((r) => setChats(r.chats))
      .catch(() => setChats([]));
  };

  useEffect(() => {
    load();
    const stopS = subscribeSessions(
      (type, id, s) =>
        setSessions((cur) => {
          if (!cur) return cur;
          if (type === "deleted" || !s) return cur.filter((x) => x.id !== id);
          return cur.some((x) => x.id === id) ? cur.map((x) => (x.id === id ? s : x)) : [s, ...cur];
        }),
      load,
    );
    const stopC = subscribeChats(
      (id, chat) =>
        setChats((cur) => {
          if (!cur) return cur;
          if (!chat) return cur.filter((c) => c.id !== id);
          return cur.some((c) => c.id === id) ? cur.map((c) => (c.id === id ? chat : c)) : [...cur, chat];
        }),
      (msg) =>
        setChats(
          (cur) =>
            cur &&
            cur.map((c) => {
              if (c.id !== msg.chat_id) return c;
              const isNew = !c.last_message || msg.id > c.last_message.id;
              const open = location.pathname === `/chats/${c.id}`;
              return { ...c, last_message: isNew ? msg : c.last_message, unread: isNew && msg.author.kind === "session" && !open ? c.unread + 1 : c.unread };
            }),
        ),
    );
    const onTheme = (e: Event) => setTheme((e as CustomEvent).detail);
    addEventListener("agents-operator:theme", onTheme);
    return () => {
      stopS();
      stopC();
      removeEventListener("agents-operator:theme", onTheme);
    };
  }, []);

  // Opening a conversation opens its tab.
  const path = route.path === "/sessions/:id" ? `/sessions/${route.params.id}` : route.path === "/chats/:id" ? `/chats/${route.params.id}` : "";
  useEffect(() => {
    if (!path || route.params.id === "new") return;
    setTabs((cur) => {
      if (cur.includes(path)) return cur;
      const next = [...cur, path].slice(-12);
      saveTabs(next);
      return next;
    });
  }, [path]);

  const closeTab = (p: string) => {
    setTabs((cur) => {
      const i = cur.indexOf(p);
      const next = cur.filter((x) => x !== p);
      saveTabs(next);
      if (p === path) navigate(next[Math.min(i, next.length - 1)] ?? "/");
      return next;
    });
  };

  const q = search.trim().toLowerCase();
  const agents = useMemo(() => {
    const list = (sessions ?? []).filter((s) => !q || `${s.name} ${s.handle} ${s.agent} ${s.persona}`.toLowerCase().includes(q));
    return list.sort((a, b) => lastActive(b).localeCompare(lastActive(a)));
  }, [sessions, q]);
  const attention = agents.filter(needsYou);
  const chatList = useMemo(() => {
    const list = (chats ?? []).filter((c) => !q || `${c.name} ${c.description}`.toLowerCase().includes(q));
    return list.sort((a, b) => (b.last_message?.created_at ?? b.created_at).localeCompare(a.last_message?.created_at ?? a.created_at));
  }, [chats, q]);
  const unreadChats = (chats ?? []).reduce((n, c) => n + (c.unread > 0 ? 1 : 0), 0);

  const current = route.path === "/sessions/:id" ? sessions?.find((s) => s.id === route.params.id) : undefined;
  const currentChat = route.path === "/chats/:id" ? chats?.find((c) => c.id === route.params.id) : undefined;
  const open = Boolean(path) || route.path === "/chats" || route.path === "/routines";
  const toggle = (k: string) => setCollapsed((c) => ({ ...c, [k]: !c[k] }));

  let content: preact.ComponentChildren;
  if (route.path === "/sessions/:id") {
    if (!sessions) content = <div class="sh-placeholder">Loading…</div>;
    else if (!current)
      content = (
        <div class="sh-placeholder">
          <h2>Not found</h2>This agent does not exist (any more).
        </div>
      );
    else if (current.interface === "conversation") content = <AgentChat key={current.id} session={current} />;
    else content = <TerminalSession session={current} />;
  } else if (route.path === "/routines") {
    content = <RoutinesPage sessions={sessions ?? []} chats={chats ?? []} />;
  } else if (route.path === "/chats/:id" || route.path === "/chats") {
    content = <Chats key={route.params.id ?? "list"} user={props.user} onLogout={props.onLogout} chatId={route.params.id === "new" ? undefined : route.params.id} embedded creating={route.params.id === "new"} />;
  } else {
    content = (
      <div class="sh-placeholder">
        <Icon name="bot" size={34} />
        <h2>Your agents</h2>
        Pick an agent or a chat on the left, or start a new agent. Every agent keeps its own workspace and conversation.
        <div style={{ marginTop: 16, display: "flex", gap: 8, justifyContent: "center" }}>
          <Link href="/new" class="sh-btn primary">
            <Icon name="plus" size={16} /> New agent
          </Link>
          <Link href="/chats/new" class="sh-btn">
            <Icon name="chats" size={16} /> New chat
          </Link>
        </div>
      </div>
    );
  }

  return (
    <div class={`sh ${open ? "sh-open" : ""}`}>
      <nav class="sh-rail">
        <Link href="/" class="logo" title="agents-operator">
          ao
        </Link>
        <Link href="/" class={`sh-rail-btn ${!route.path.startsWith("/chats") && route.path !== "/routines" ? "active" : ""}`} title="Agents">
          <Icon name="bot" size={20} />
          {attention.length > 0 && <span class="dot" />}
        </Link>
        <Link href={chatList[0] ? `/chats/${chatList[0].id}` : "/chats/new"} class={`sh-rail-btn ${route.path.startsWith("/chats") ? "active" : ""}`} title="Chats">
          <Icon name="chats" size={20} />
          {unreadChats > 0 && <span class="dot" />}
        </Link>
        <Link href="/routines" class={`sh-rail-btn ${route.path === "/routines" ? "active" : ""}`} title="Routines">
          <Icon name="clock" size={20} />
        </Link>
        <Link href="/new" class="sh-rail-btn" title="New agent">
          <Icon name="plus" size={20} />
        </Link>
        <span class="spacer" />
        <button class="sh-rail-btn" title="Theme" onClick={() => applyTheme(theme === "dark" ? "light" : "dark")}>
          <Icon name={theme === "dark" ? "sun" : "moon"} size={19} />
        </button>
        <Link href="/account" class="sh-rail-btn" title={`Account (${props.user.username})`}>
          <Icon name="sliders" size={19} />
        </Link>
        <button class="sh-rail-btn" title="Log out" onClick={props.onLogout}>
          <Icon name="logout" size={19} />
        </button>
      </nav>

      <aside class="sh-side">
        <div class="sh-side-head">
          <span class="title">Agents</span>
          <Link href="/chats/new" class="sh-icon-btn" title="New chat">
            <Icon name="chats" size={17} />
          </Link>
          <Link href="/new" class="sh-icon-btn" title="New agent">
            <Icon name="plus" size={18} />
          </Link>
        </div>
        <label class="sh-search">
          <Icon name="search" size={16} />
          <input placeholder="Search" value={search} onInput={(e) => setSearch((e.target as HTMLInputElement).value)} />
          {search && (
            <button class="sh-icon-btn" style={{ width: 22, height: 22 }} onClick={() => setSearch("")} title="Clear">
              <Icon name="x" size={14} />
            </button>
          )}
        </label>
        <div class="sh-list">
          {error && <div class="sh-empty-list">{error}</div>}
          {attention.length > 0 && (
            <>
              <button class="sh-section" onClick={() => toggle("needs")}>
                Needs you <span class="count">{attention.length}</span>
                <span class="grow" />
                <Icon name={collapsed.needs ? "chevronRight" : "chevronDown"} size={14} />
              </button>
              {!collapsed.needs && attention.map((s) => <AgentRow s={s} active={s.id === current?.id} />)}
            </>
          )}
          <button class="sh-section" onClick={() => toggle("agents")}>
            Agents
            <span class="grow" />
            <Icon name={collapsed.agents ? "chevronRight" : "chevronDown"} size={14} />
          </button>
          {!collapsed.agents && (
            <>
              {sessions === null && <div class="sh-empty-list">Loading…</div>}
              {sessions?.length === 0 && (
                <div class="sh-empty-list">
                  No agents yet. <Link href="/new">Create one</Link>: it gets its own workspace, and you talk to it here.
                </div>
              )}
              {agents.map((s) => (
                <AgentRow s={s} active={s.id === current?.id} />
              ))}
            </>
          )}
          <button class="sh-section" onClick={() => toggle("chats")}>
            Chats
            <span class="grow" />
            <Icon name={collapsed.chats ? "chevronRight" : "chevronDown"} size={14} />
          </button>
          {!collapsed.chats && (
            <>
              {chats?.length === 0 && (
                <div class="sh-empty-list">
                  No chats yet. <Link href="/chats/new">Start one</Link> and add agents: they work together there.
                </div>
              )}
              {chatList.map((c) => (
                <ChatRow c={c} active={c.id === currentChat?.id} />
              ))}
            </>
          )}
        </div>
        <div class="sh-side-foot">
          <button class="sh-icon-btn" title="Theme" onClick={() => applyTheme(theme === "dark" ? "light" : "dark")}>
            <Icon name={theme === "dark" ? "sun" : "moon"} size={18} />
          </button>
          <Link href="/account" class="sh-icon-btn" title="Account">
            <Icon name="sliders" size={18} />
          </Link>
          <Link href="/routines" class="sh-icon-btn" title="Routines">
            <Icon name="clock" size={18} />
          </Link>
          <span style={{ flex: 1 }} />
          <button class="sh-icon-btn" title="Log out" onClick={props.onLogout}>
            <Icon name="logout" size={18} />
          </button>
        </div>
      </aside>

      <main class="sh-main">
        <div class="sh-topbar">
          <Link href="/" class="sh-icon-btn sh-back" title="Back">
            <Icon name="chevronLeft" size={20} />
          </Link>
          <div class="sh-tabs">
            {tabs.map((t) => {
              const [, kind, id] = t.split("/");
              const s = kind === "sessions" ? sessions?.find((x) => x.id === id) : undefined;
              const c = kind === "chats" ? chats?.find((x) => x.id === id) : undefined;
              if (!s && !c) return null;
              return (
                <Link href={t} class={`sh-tab ${t === path ? "active" : ""}`}>
                  {s ? <Avatar id={s.id} size={18} /> : <Icon name="chats" size={16} />}
                  <span class="name">{s?.name ?? c?.name}</span>
                  <button
                    class="close"
                    title="Close tab"
                    onClick={(e) => {
                      e.preventDefault();
                      e.stopPropagation();
                      closeTab(t);
                    }}
                  >
                    <Icon name="x" size={13} />
                  </button>
                </Link>
              );
            })}
          </div>
          <div id="sh-topbar-slot" class="sh-slot" />
        </div>
        <div class="sh-content">{content}</div>
      </main>
    </div>
  );
}

function AgentRow({ s, active }: { s: Session; active: boolean }) {
  const status = agentStatus(s);
  const line = agentLine(s);
  return (
    <Link href={`/sessions/${s.id}`} class={`sh-row ${active ? "active" : ""}`} title={`${s.name} (@${s.handle})`}>
      <Avatar id={s.id} size={40} status={status} />
      <div class="sh-row-body">
        <div class="sh-row-top">
          <span class="sh-row-name">{s.name}</span>
          <span class="sh-tag">{roleTag(s)}</span>
          <span class="sh-row-time">{shortTime(lastActive(s))}</span>
        </div>
        <div class="sh-row-sub">
          <span class={`text ${status === "working" ? "working" : ""}`}>{line}</span>
        </div>
      </div>
    </Link>
  );
}

function ChatRow({ c, active }: { c: Chat; active: boolean }) {
  const m = c.last_message;
  const who = m ? (m.author.kind === "owner" ? "You" : m.author.name) : "";
  const text = m ? m.text.replace(/\s+/g, " ").trim() || (m.attachments.length ? "📎 file" : "") : "No messages yet";
  const members = c.members.slice(0, 3);
  return (
    <Link href={`/chats/${c.id}`} class={`sh-row ${active ? "active" : ""}`}>
      {members.length ? (
        <span class="sh-stack">
          {members.map((mb) => (
            <Avatar id={mb.session_id} size={members.length > 1 ? 28 : 40} />
          ))}
        </span>
      ) : (
        <span class="sh-chat-icon">
          <Icon name="chats" size={18} />
        </span>
      )}
      <div class="sh-row-body">
        <div class="sh-row-top">
          <span class="sh-row-name">{c.name}</span>
          {c.muted && <span class="sh-tag">muted</span>}
          <span class="sh-row-time">{shortTime(m?.created_at ?? c.created_at)}</span>
        </div>
        <div class="sh-row-sub">
          <span class="text">{m ? `${who}: ${text}` : text}</span>
          {c.unread > 0 && <span class="sh-unread">{c.unread}</span>}
        </div>
      </div>
    </Link>
  );
}

/** A session still in terminal mode: open its terminal or switch it. */
function TerminalSession({ session }: { session: Session }) {
  const canConvert = session.agent !== "shell";
  return (
    <div class="sh-placeholder">
      <Avatar id={session.id} size={56} status={agentStatus(session)} />
      <h2>{session.name}</h2>
      {canConvert
        ? "This agent still runs in a terminal. Switch it to a conversation (in its settings, while it is stopped) to chat with it here."
        : "A shell session: it only has a terminal."}
      <div style={{ marginTop: 16, display: "flex", gap: 8, justifyContent: "center", flexWrap: "wrap" }}>
        <Link href={`/sessions/${session.id}/terminal`} class="sh-btn primary">
          <Icon name="terminal" size={16} /> Open terminal
        </Link>
        {canConvert && (
          <Link href={`/sessions/${session.id}/edit`} class="sh-btn">
            <Icon name="sliders" size={16} /> Settings
          </Link>
        )}
      </div>
    </div>
  );
}
