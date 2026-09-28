import { useEffect, useState } from "preact/hooks";
import { api, subscribeSessions, type Session, type User } from "../api";
import { Nav } from "../components/Nav";
import { Link } from "../router";
import { reposSummary, stateLabel, timeAgo } from "../util";

export function SessionList(props: { user: User; onLogout: () => void }) {
  const [sessions, setSessions] = useState<Session[] | null>(null);
  const [error, setError] = useState("");
  const [, tick] = useState(0);

  const load = () =>
    api
      .sessions()
      .then((r) => {
        setSessions(r.sessions);
        setError("");
      })
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    load();
    const stop = subscribeSessions(
      (type, id, session) => {
        setSessions((cur) => {
          if (!cur) return cur;
          if (type === "deleted") return cur.filter((s) => s.id !== id);
          if (!session) return cur;
          const idx = cur.findIndex((s) => s.id === id);
          if (idx < 0) return [session, ...cur];
          const next = cur.slice();
          next[idx] = session;
          return next;
        });
      },
      load, // reload on (re)connect so nothing is missed
    );
    const t = setInterval(() => tick((n) => n + 1), 15000);
    return () => {
      stop();
      clearInterval(t);
    };
  }, []);

  return (
    <div class="page">
      <Nav
        user={props.user}
        onLogout={props.onLogout}
        right={
          <Link href="/new" class="btn primary small">
            + New session
          </Link>
        }
      />
      {error && <div class="error">{error}</div>}
      {sessions === null ? (
        <div class="muted">Loading…</div>
      ) : sessions.length === 0 ? (
        <div class="card empty">
          No sessions yet. <Link href="/new">Create one</Link> to start an agent in its own pod.
        </div>
      ) : (
        <div class="sessions">
          {sessions.map((s) => (
            <SessionCard key={s.id} s={s} />
          ))}
        </div>
      )}
    </div>
  );
}

function SessionCard({ s }: { s: Session }) {
  const exited = s.state === "running" && s.agent_running === false;
  return (
    <Link href={`/sessions/${s.id}`} class="card session-card">
      <span class={`dot ${s.state}`} title={stateLabel(s.state)} />
      <div style="min-width:0">
        <div class="title">
          {s.name}
          <span class="badge agent">{s.agent}</span>
          {s.needs_attention && <span class="badge attention">needs you</span>}
          {exited && <span class="badge">agent exited {s.exit_code !== undefined ? `(${s.exit_code})` : ""}</span>}
          {s.state !== "running" && <span class="badge">{stateLabel(s.state)}</span>}
        </div>
        <div class="sub">
          {reposSummary(s.repos)} · output {timeAgo(s.last_output_at)}
          {s.state_reason ? ` · ${s.state_reason}` : ""}
        </div>
        {s.tail && <div class="tail">{s.tail}</div>}
      </div>
      <span class="muted" style="font-size:13px">
        {s.clients > 0 ? `${s.clients} attached` : ""}
      </span>
    </Link>
  );
}
