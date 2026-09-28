import { useEffect, useRef, useState } from "preact/hooks";
import { api, subscribeSessions, type Session, type SessionEvent } from "../api";
import { Link, navigate } from "../router";
import { Terminal, type TerminalHandle } from "../components/Terminal";
import { KeyBar } from "../components/KeyBar";
import { stateLabel, timeAgo } from "../util";
import { toggleTheme } from "../theme";

export function SessionPage({ id }: { id: string }) {
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [drawer, setDrawer] = useState<null | "events" | "logs" | "info">(null);
  const term = useRef<TerminalHandle>(null);

  const load = () =>
    api
      .session(id)
      .then((s) => {
        setSession(s);
        setError("");
      })
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    load();
    const stop = subscribeSessions((type, sid, s) => {
      if (sid !== id) return;
      if (type === "deleted") navigate("/", true);
      else if (s) setSession(s);
    }, load);
    return stop;
  }, [id]);

  const act = async (what: string, fn: () => Promise<unknown>) => {
    setBusy(what);
    setError("");
    try {
      await fn();
      await load();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy("");
    }
  };

  const del = () => {
    if (!session) return;
    if (!confirm(`Delete "${session.name}" and its volume? This cannot be undone.`)) return;
    act("delete", () => api.deleteSession(id).then(() => navigate("/", true)));
  };

  const saveLogin = () => {
    if (!session) return;
    const kind = session.agent === "codex" ? "codex_login" : "claude_login";
    act("save-login", () => api.saveLogin(id, kind));
  };

  const s = session;
  const canAttach = s?.state === "running";

  return (
    <div class="term-page">
      <div class="topbar">
        <Link href="/" class="btn small" title="Back to sessions">
          ←
        </Link>
        <span class={`dot ${s?.state ?? ""}`} />
        <span class="name">{s?.name ?? "…"}</span>
        {s && <span class="badge agent">{s.agent}</span>}
        {s && s.state !== "running" && <span class="badge">{stateLabel(s.state)}</span>}
        {s?.needs_attention && <span class="badge attention">needs you</span>}
        {s?.agent_running === false && s.state === "running" && (
          <span class="badge">exited {s.exit_code !== undefined ? s.exit_code : ""}</span>
        )}
        <span class="grow" />
        {s && (s.state === "stopped" || s.state === "failed") && (
          <button class="btn small primary" disabled={!!busy} onClick={() => act("start", () => api.startSession(id))}>
            Start
          </button>
        )}
        {s && (s.state === "running" || s.state === "creating") && (
          <button class="btn small" disabled={!!busy} onClick={() => act("stop", () => api.stopSession(id))}>
            Stop
          </button>
        )}
        {canAttach && (
          <button class="btn small" disabled={!!busy} onClick={() => act("restart", () => api.restartAgent(id))} title="Relaunch the agent CLI in the same pod">
            Restart agent
          </button>
        )}
        {canAttach && s.agent !== "shell" && s.agent !== "opencode" && (
          <button class="btn small" disabled={!!busy} onClick={saveLogin} title="Copy the CLI's login file to your account">
            Save login
          </button>
        )}
        <button class="btn small" onClick={() => setDrawer(drawer ? null : "events")} title="Events and logs">
          ☰
        </button>
        <button class="btn small" onClick={toggleTheme} title="Toggle theme">
          ◐
        </button>
        <button class="btn small danger" disabled={!!busy} onClick={del}>
          Delete
        </button>
      </div>

      <div class="term-wrap">
        {error && <div class="banner error">{error}</div>}
        {canAttach ? (
          <Terminal ref={term} sessionId={id} />
        ) : (
          <div class="overlay">
            {s ? (
              <>
                <div style="font-size:18px">{stateLabel(s.state)}</div>
                {s.state_reason && <div class="mono" style="font-size:13px">{s.state_reason}</div>}
                {(s.state === "creating" || s.state === "stopping" || s.state === "deleting") && <div>working…</div>}
                {(s.state === "stopped" || s.state === "failed") && (
                  <div>
                    The volume is kept. Press <b>Start</b> to recreate the pod.
                    {s.state === "failed" && (
                      <>
                        {" "}
                        <button class="btn small" onClick={() => setDrawer("logs")}>
                          view pod logs
                        </button>
                      </>
                    )}
                  </div>
                )}
              </>
            ) : (
              <div>{error || "Loading…"}</div>
            )}
          </div>
        )}
        {drawer && s && <Drawer session={s} tab={drawer} setTab={setDrawer} onClose={() => setDrawer(null)} />}
      </div>

      {canAttach && (
        <KeyBar
          onKey={(seq) => term.current?.send(seq)}
          onFocus={() => term.current?.focus()}
        />
      )}
    </div>
  );
}

function Drawer(props: { session: Session; tab: "events" | "logs" | "info"; setTab: (t: "events" | "logs" | "info") => void; onClose: () => void }) {
  const [events, setEvents] = useState<SessionEvent[]>([]);
  const [logs, setLogs] = useState("");
  const [err, setErr] = useState("");
  const s = props.session;

  useEffect(() => {
    setErr("");
    if (props.tab === "events") api.sessionEvents(s.id).then((r) => setEvents(r.events)).catch((e) => setErr((e as Error).message));
    if (props.tab === "logs") api.sessionLogs(s.id).then(setLogs).catch((e) => setErr((e as Error).message));
  }, [props.tab, s.id, s.state]);

  return (
    <div class="drawer">
      <header>
        <div class="tabs">
          {(["events", "logs", "info"] as const).map((t) => (
            <button class={`btn small ${props.tab === t ? "primary" : ""}`} onClick={() => props.setTab(t)}>
              {t}
            </button>
          ))}
        </div>
        <span class="grow" style="flex:1" />
        <button class="btn small" onClick={props.onClose}>
          ✕
        </button>
      </header>
      <div class="body">
        {err && <div class="error">{err}</div>}
        {props.tab === "events" && (
          <div class="events">
            <ul>
              {events.map((e) => (
                <li>
                  <span class="when">
                    {timeAgo(e.at)} · {e.kind}
                  </span>
                  <div>{e.message}</div>
                </li>
              ))}
              {events.length === 0 && <li class="muted">no events</li>}
            </ul>
          </div>
        )}
        {props.tab === "logs" && <pre>{logs || "(no output)"}</pre>}
        {props.tab === "info" && (
          <pre>
            {[
              `id:            ${s.id}`,
              `pod:           ${s.pod_name}`,
              `state:         ${s.state}${s.state_reason ? " (" + s.state_reason + ")" : ""}`,
              `agent:         ${s.agent}${s.autonomous ? " (autonomous)" : ""}`,
              ...(s.repos?.length
                ? s.repos.map((r, i) => `${i === 0 ? "repos:         " : "               "}/workspace/${r.path}  ←  ${r.url}${r.branch ? " @ " + r.branch : ""}`)
                : ["repos:         (none)"]),
              `image tag:     ${s.image_tag || "(default)"}`,
              `pvc:           ${s.pvc_size} ${s.storage_class}`,
              `limits:        cpu ${s.resources?.limits?.cpu || "default"}, memory ${s.resources?.limits?.memory || "default"}`,
              `created:       ${s.created_at}`,
              `last attached: ${s.last_attached_at || "never"}`,
              `last output:   ${s.last_output_at || "never"}`,
              `clients:       ${s.clients}`,
              s.runner_error ? `runner error:  ${s.runner_error}` : "",
              Object.keys(s.env || {}).length ? `env:           ${Object.keys(s.env).join(", ")}` : "",
            ]
              .filter(Boolean)
              .join("\n")}
          </pre>
        )}
      </div>
    </div>
  );
}
