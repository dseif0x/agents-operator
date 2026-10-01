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
  const [sheet, setSheet] = useState<null | "select" | "links">(null);
  const [notice, setNotice] = useState("");
  const term = useRef<TerminalHandle>(null);
  const page = useRef<HTMLDivElement>(null);

  // Phones. The on-screen keyboard only shrinks the visual viewport, not a
  // position:fixed layout, so the page takes its height from the visual
  // viewport: the terminal shrinks to what is visible, the PTY is resized
  // and the TUI lays itself out for the smaller screen. A pinch-zoomed
  // viewport is left alone, and the document itself is never allowed to
  // stay scrolled: a page that Safari left panned after a keyboard went
  // away (and restored that way on reload) is one where a tap on the
  // terminal no longer brings the keyboard back.
  useEffect(() => {
    const vv = window.visualViewport;
    if (!vv) return;
    // Relative to the scale the page loaded at: Safari's per-site page zoom
    // is reported in `scale` as well and is not a pinch.
    const baseScale = vv.scale;
    const zoomed = () => vv.scale > baseScale * 1.1;
    const apply = () => {
      const el = page.current;
      if (!el || zoomed()) return;
      el.style.height = `${Math.round(vv.height)}px`;
    };
    // If Safari still pans the visual viewport (a drag that started on the
    // top bar, say), snap it back instead of following it.
    const snap = () => {
      if (zoomed()) return;
      if (vv.offsetTop > 0 || window.scrollY > 0) window.scrollTo(0, 0);
    };
    window.scrollTo(0, 0);
    apply();
    vv.addEventListener("resize", apply);
    vv.addEventListener("scroll", snap);
    return () => {
      vv.removeEventListener("resize", apply);
      vv.removeEventListener("scroll", snap);
    };
  }, []);

  const flash = (msg: string) => {
    setNotice(msg);
    setTimeout(() => setNotice(""), 1800);
  };

  const copy = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      flash("Copied");
    } catch {
      flash("Copy failed; long-press the text instead");
    }
  };

  const pasteFromClipboard = async () => {
    try {
      const text = await navigator.clipboard.readText();
      if (text) {
        term.current?.paste(text);
        term.current?.focus();
      } else {
        flash("Clipboard is empty");
      }
    } catch {
      flash("Clipboard access denied; allow paste for this site");
    }
  };

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
    <div class="term-page" ref={page}>
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
          <>
            <button class="btn small primary" disabled={!!busy} onClick={() => act("start", () => api.startSession(id))}>
              Start
            </button>
            <Link href={`/sessions/${id}/edit`} class="btn small" title="Change the settings the next start uses">
              Edit
            </Link>
          </>
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
        {sheet === "select" && (
          <TextSheet
            title="Screen text"
            text={term.current?.selection() || term.current?.screenText() || ""}
            onCopy={copy}
            onClose={() => setSheet(null)}
          />
        )}
        {sheet === "links" && <LinksSheet links={term.current?.links() ?? []} onCopy={copy} onClose={() => setSheet(null)} />}
        {notice && <div class="toast">{notice}</div>}
      </div>

      {canAttach && (
        <KeyBar
          onKey={(seq) => term.current?.send(seq)}
          terminalFocused={() => term.current?.hasFocus() ?? false}
          refocus={() => term.current?.focus()}
          onPaste={pasteFromClipboard}
          onSelect={() => setSheet(sheet === "select" ? null : "select")}
          onLinks={() => setSheet(sheet === "links" ? null : "links")}
        />
      )}
    </div>
  );
}

// TextSheet shows terminal text in a natively selectable box, because xterm
// swallows touch selection gestures on phones.
function TextSheet(props: { title: string; text: string; onCopy: (t: string) => void; onClose: () => void }) {
  return (
    <div class="sheet">
      <header>
        <b>{props.title}</b>
        <span style="flex:1" />
        <button class="btn small" onClick={() => props.onCopy(props.text)}>
          Copy all
        </button>
        <button class="btn small" onClick={props.onClose}>
          ✕
        </button>
      </header>
      <pre class="selectable">{props.text || "(nothing on screen)"}</pre>
    </div>
  );
}

function LinksSheet(props: { links: string[]; onCopy: (t: string) => void; onClose: () => void }) {
  return (
    <div class="sheet">
      <header>
        <b>Links on screen</b>
        <span style="flex:1" />
        <button class="btn small" onClick={props.onClose}>
          ✕
        </button>
      </header>
      <div class="body">
        {props.links.length === 0 && <div class="muted">No links on screen.</div>}
        {props.links.map((l) => (
          <div class="link-row">
            <a href={l} target="_blank" rel="noopener noreferrer" class="mono">
              {l}
            </a>
            <button class="btn small" onClick={() => props.onCopy(l)}>
              Copy
            </button>
          </div>
        ))}
      </div>
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
              `limits:        cpu ${s.resources?.limits?.cpu || "default"}, memory ${s.resources?.limits?.memory || "default"}${Object.entries(
                s.resources?.limits || {},
              )
                .filter(([k, v]) => k !== "cpu" && k !== "memory" && v)
                .map(([k, v]) => `, ${k} ${v}`)
                .join("")}`,
              `runtime class: ${s.runtime_class || "(chart default)"}`,
              `k8s access:    ${s.service_account ? "read-only ServiceAccount mounted" : "none"}`,
              Object.keys(s.node_selector || {}).length
                ? `node selector: ${Object.entries(s.node_selector)
                    .map(([k, v]) => `${k}=${v}`)
                    .join(", ")}`
                : "",
              s.tolerations?.length
                ? `tolerations:   ${s.tolerations
                    .map((t) => `${t.key || "*"}${t.operator === "Equal" ? "=" + (t.value ?? "") : ""}${t.effect ? ":" + t.effect : ""}${t.tolerationSeconds !== undefined ? "/" + t.tolerationSeconds : ""}`)
                    .join(", ")}`
                : "",
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
