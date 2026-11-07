// Routines: instructions an agent or a chat gets on a schedule or when a
// webhook arrives. The list and editor live in an agent's details panel and
// on the Routines page, which also shows the week ahead.
import { useEffect, useMemo, useState } from "preact/hooks";
import { api, type CalendarEntry, type Chat, type Routine, type RoutineInput, type RoutineRun, type Schedule, type Session } from "../api";
import { Link } from "../router";
import { Avatar } from "./Avatar";
import { Icon } from "./icons";
import { shortTime } from "./format";

const localTZ = (() => {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
})();

const dayNames = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const monthNames = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

const kinds: { value: Schedule["kind"]; label: string }[] = [
  { value: "daily", label: "Daily" },
  { value: "weekdays", label: "Weekdays" },
  { value: "weekly", label: "Weekly" },
  { value: "monthly", label: "Monthly" },
  { value: "yearly", label: "Yearly" },
  { value: "hourly", label: "Hourly" },
  { value: "every_n_hours", label: "Every n hours" },
  { value: "once", label: "Once" },
  { value: "cron", label: "Custom (cron)" },
];

function todayISO(): string {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

/** The schedule picker: "Weekly on Thu at 08:20", built from chips. */
export function SchedulePicker({ value, onChange }: { value: Schedule; onChange: (s: Schedule) => void }) {
  const set = (patch: Partial<Schedule>) => onChange({ ...value, ...patch });
  const minuteOnly = value.kind === "hourly" || value.kind === "every_n_hours";
  return (
    <div class="rt-picker">
      <div class="rt-chips">
        <span class="rt-clock">
          <Icon name="clock" size={15} />
        </span>
        <select
          value={value.kind}
          onChange={(e) => {
            const kind = (e.target as HTMLSelectElement).value as Schedule["kind"];
            const next: Schedule = { kind, at: value.at ?? "09:00", tz: value.tz ?? localTZ };
            if (kind === "weekly") next.days = value.days?.length ? value.days : [new Date().getDay()];
            if (kind === "monthly") next.day = value.day ?? new Date().getDate();
            if (kind === "yearly") {
              next.month = value.month ?? new Date().getMonth() + 1;
              next.day = value.day ?? new Date().getDate();
            }
            if (kind === "every_n_hours") next.every = value.every ?? 2;
            if (kind === "once") next.date = value.date ?? todayISO();
            if (kind === "cron") next.cron = value.cron ?? "0 9 * * 1-5";
            onChange(next);
          }}
        >
          {kinds.map((k) => (
            <option value={k.value}>{k.label}</option>
          ))}
        </select>
        {value.kind === "weekly" && (
          <>
            <span class="rt-word">on</span>
            <span class="rt-days">
              {dayNames.map((d, i) => (
                <button
                  type="button"
                  class={value.days?.includes(i) ? "on" : ""}
                  onClick={() => {
                    const cur = value.days ?? [];
                    const next = cur.includes(i) ? cur.filter((x) => x !== i) : [...cur, i].sort();
                    if (next.length) set({ days: next });
                  }}
                >
                  {d.slice(0, 2)}
                </button>
              ))}
            </span>
          </>
        )}
        {value.kind === "monthly" && (
          <>
            <span class="rt-word">on day</span>
            <input type="number" min={1} max={31} value={value.day ?? 1} onInput={(e) => set({ day: Number((e.target as HTMLInputElement).value) })} />
          </>
        )}
        {value.kind === "yearly" && (
          <>
            <span class="rt-word">on</span>
            <select value={value.month ?? 1} onChange={(e) => set({ month: Number((e.target as HTMLSelectElement).value) })}>
              {monthNames.map((m, i) => (
                <option value={i + 1}>{m}</option>
              ))}
            </select>
            <input type="number" min={1} max={31} value={value.day ?? 1} onInput={(e) => set({ day: Number((e.target as HTMLInputElement).value) })} />
          </>
        )}
        {value.kind === "every_n_hours" && (
          <>
            <span class="rt-word">every</span>
            <input type="number" min={1} max={23} value={value.every ?? 2} onInput={(e) => set({ every: Number((e.target as HTMLInputElement).value) })} />
            <span class="rt-word">hours</span>
          </>
        )}
        {value.kind === "once" && (
          <>
            <span class="rt-word">on</span>
            <input type="date" value={value.date ?? todayISO()} onInput={(e) => set({ date: (e.target as HTMLInputElement).value })} />
          </>
        )}
        {value.kind === "cron" ? (
          <input class="rt-cron" value={value.cron ?? ""} placeholder="m h dom mon dow" onInput={(e) => set({ cron: (e.target as HTMLInputElement).value })} />
        ) : minuteOnly ? (
          <>
            <span class="rt-word">at minute</span>
            <input
              type="number"
              min={0}
              max={59}
              value={Number((value.at ?? "00:00").split(":")[1])}
              onInput={(e) => set({ at: `00:${String((e.target as HTMLInputElement).value).padStart(2, "0")}` })}
            />
          </>
        ) : (
          <>
            <span class="rt-word">at</span>
            <input type="time" value={value.at ?? "09:00"} onInput={(e) => set({ at: (e.target as HTMLInputElement).value })} />
          </>
        )}
      </div>
      <div class="rt-hint">
        {describe(value)} · {value.tz || "UTC"}
        {value.tz !== localTZ && (
          <button type="button" class="rt-link" onClick={() => set({ tz: localTZ })}>
            use {localTZ}
          </button>
        )}
      </div>
    </div>
  );
}

/** The schedule in words, as the hub says it. */
export function describe(s: Schedule): string {
  const at = s.at ?? "09:00";
  const fullDays = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
  switch (s.kind) {
    case "once":
      return `Once on ${s.date} at ${at}`;
    case "hourly":
      return `Every hour at :${at.split(":")[1]}`;
    case "every_n_hours":
      return `Every ${s.every} hours at :${at.split(":")[1]}`;
    case "daily":
      return `Every day at ${at}`;
    case "weekdays":
      return `Weekdays at ${at}`;
    case "weekly":
      return `Every ${(s.days ?? []).map((d) => fullDays[d]).join(", ")} at ${at}`;
    case "monthly":
      return `Monthly on day ${s.day} at ${at}`;
    case "yearly":
      return `Yearly on ${monthNames[(s.month ?? 1) - 1]} ${s.day} at ${at}`;
    case "cron":
      return `Cron ${s.cron}`;
  }
}

const runLabel: Record<RoutineRun["status"], string> = {
  queued: "Waiting",
  running: "Running",
  succeeded: "Done",
  failed: "Failed",
  interrupted: "Stopped",
  sent: "Posted",
};

export function RunBadge({ run }: { run: RoutineRun }) {
  const cls = run.status === "failed" ? "bad" : run.status === "succeeded" || run.status === "sent" ? "ok" : run.status === "interrupted" ? "" : "busy";
  return (
    <span class={`rt-badge ${cls}`}>
      {run.status === "running" || run.status === "queued" ? <span class="cv-spin" /> : <Icon name={cls === "bad" ? "x" : cls === "ok" ? "check" : "stop"} size={12} />}
      {run.quiet ? "No update" : runLabel[run.status]}
    </span>
  );
}

/** The routines of one agent or chat, with an editor. */
export function RoutineList({ sessionId, chatId }: { sessionId?: string; chatId?: string }) {
  const [routines, setRoutines] = useState<Routine[] | null>(null);
  const [editing, setEditing] = useState<Routine | "new" | null>(null);
  const [err, setErr] = useState("");
  const load = () =>
    api
      .routines({ session: sessionId, chat: chatId })
      .then((r) => setRoutines(r.routines))
      .catch((e) => setErr((e as Error).message));
  useEffect(() => {
    load();
  }, [sessionId, chatId]);
  if (editing)
    return (
      <RoutineEditor
        routine={editing === "new" ? null : editing}
        target={{ session_id: sessionId, chat_id: chatId }}
        onDone={() => {
          setEditing(null);
          load();
        }}
      />
    );
  return (
    <div class="rt-list">
      <div class="rt-list-head">
        <span>Routines</span>
        <button class="sh-icon-btn" title="New routine" onClick={() => setEditing("new")}>
          <Icon name="plus" size={17} />
        </button>
      </div>
      {err && <div class="cv-error">{err}</div>}
      {routines?.length === 0 && (
        <button class="rt-empty" onClick={() => setEditing("new")}>
          <Icon name="clock" size={16} />
          <span>Run something on a schedule or when a webhook arrives</span>
        </button>
      )}
      {routines?.map((r) => (
        <RoutineRow r={r} onOpen={() => setEditing(r)} onChanged={load} />
      ))}
    </div>
  );
}

function RoutineRow({ r, onOpen, onChanged }: { r: Routine; onOpen: () => void; onChanged: () => void }) {
  return (
    <div class="rt-row" onClick={onOpen}>
      <Icon name={r.trigger === "webhook" ? "plug" : "clock"} size={16} class={r.enabled ? "cv-ok" : ""} />
      <div class="rt-row-body">
        <div class="rt-row-name">{r.name}</div>
        <div class="rt-row-sub">
          {r.describe}
          {r.enabled && r.next_run_at ? ` · next ${shortTime(r.next_run_at)}` : ""}
        </div>
      </div>
      {r.last_run && <RunBadge run={r.last_run} />}
      <Toggle
        on={r.enabled}
        onChange={async (on) => {
          await api.updateRoutine(r.id, { enabled: on });
          onChanged();
        }}
      />
    </div>
  );
}

function Toggle({ on, onChange }: { on: boolean; onChange: (on: boolean) => void }) {
  return (
    <button
      type="button"
      class={`rt-toggle ${on ? "on" : ""}`}
      role="switch"
      aria-checked={on}
      onClick={(e) => {
        e.stopPropagation();
        onChange(!on);
      }}
    >
      <span />
    </button>
  );
}

/** Create or edit one routine. */
export function RoutineEditor({ routine, target, onDone }: { routine: Routine | null; target: { session_id?: string; chat_id?: string }; onDone: () => void }) {
  const [name, setName] = useState(routine?.name ?? "");
  const [instruction, setInstruction] = useState(routine?.instruction ?? "");
  const [trigger, setTrigger] = useState<Routine["trigger"]>(routine?.trigger ?? "schedule");
  const [schedule, setSchedule] = useState<Schedule>(routine?.schedule ?? { kind: "weekdays", at: "09:00", tz: localTZ });
  const [eventType, setEventType] = useState(routine?.event_type ?? "");
  const [filters, setFilters] = useState(routine?.filters ?? []);
  const [secret, setSecret] = useState(routine?.secret ?? "");
  const [saved, setSaved] = useState<Routine | null>(routine);
  const [runs, setRuns] = useState<RoutineRun[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [note, setNote] = useState("");

  const loadRuns = (id: string) => api.routineRuns(id).then((r) => setRuns(r.runs)).catch(() => undefined);
  useEffect(() => {
    if (saved) loadRuns(saved.id);
  }, [saved?.id]);

  const save = async () => {
    setBusy(true);
    setErr("");
    const body: RoutineInput = { name, instruction, trigger, schedule: trigger === "schedule" ? schedule : undefined, event_type: eventType, filters };
    try {
      const r = saved ? await api.updateRoutine(saved.id, body) : await api.createRoutine({ ...body, ...target });
      setSaved(r);
      if (r.secret) setSecret(r.secret);
      setNote("Saved");
      setTimeout(() => setNote(""), 1500);
      if (!saved && r.trigger === "schedule") onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const hookURL = saved?.webhook_path ? `${location.origin}${saved.webhook_path}` : "";
  const copy = (text: string) => navigator.clipboard?.writeText(text).then(() => setNote("Copied"));

  return (
    <div class="rt-editor">
      <div class="rt-list-head">
        <button class="sh-icon-btn" title="Back" onClick={onDone}>
          <Icon name="chevronLeft" size={18} />
        </button>
        <span>{saved ? saved.name : "New routine"}</span>
        <span style={{ width: 30 }} />
      </div>
      {err && <div class="cv-error">{err}</div>}
      <label class="rt-label">Name</label>
      <input class="rt-input" value={name} placeholder="Morning brief" onInput={(e) => setName((e.target as HTMLInputElement).value)} />
      <label class="rt-label">Instruction</label>
      <textarea
        class="rt-input"
        rows={5}
        value={instruction}
        placeholder="Look at the open pull requests and summarise what needs me today."
        onInput={(e) => setInstruction((e.target as HTMLTextAreaElement).value)}
      />
      <label class="rt-label">When</label>
      <div class="rt-seg">
        <button type="button" class={trigger === "schedule" ? "on" : ""} onClick={() => setTrigger("schedule")}>
          <Icon name="clock" size={14} /> Schedule
        </button>
        <button type="button" class={trigger === "webhook" ? "on" : ""} onClick={() => setTrigger("webhook")}>
          <Icon name="plug" size={14} /> Webhook
        </button>
      </div>
      {trigger === "schedule" ? (
        <SchedulePicker value={schedule} onChange={setSchedule} />
      ) : (
        <div class="rt-hook">
          {saved?.trigger === "webhook" && hookURL ? (
            <>
              <div class="rt-hint">POST JSON to this URL, signed with the secret (header X-Hub-Signature-256: sha256=HMAC of the body), or with Authorization: Bearer &lt;secret&gt;.</div>
              <div class="rt-copy">
                <code>{hookURL}</code>
                <button class="sh-icon-btn" title="Copy" onClick={() => copy(hookURL)}>
                  <Icon name="copy" size={15} />
                </button>
              </div>
              {secret ? (
                <div class="rt-copy secret">
                  <code>{secret}</code>
                  <button class="sh-icon-btn" title="Copy" onClick={() => copy(secret)}>
                    <Icon name="copy" size={15} />
                  </button>
                </div>
              ) : null}
              <div class="rt-hint">
                {secret ? "Copy the secret now: it is shown only once. " : "The secret was shown once when it was made. "}
                <button
                  type="button"
                  class="rt-link"
                  onClick={async () => {
                    if (secret || confirm("Make a new secret? The old one stops working.")) {
                      const r = await api.rotateSecret(saved.id);
                      setSecret(r.secret ?? "");
                    }
                  }}
                >
                  New secret
                </button>
              </div>
            </>
          ) : (
            <div class="rt-hint">Save the routine to get its URL and secret.</div>
          )}
          <label class="rt-label">Event type (optional)</label>
          <input class="rt-input" value={eventType} placeholder="push, deployment.created, …" onInput={(e) => setEventType((e.target as HTMLInputElement).value)} />
          <div class="rt-hint">Taken from the X-GitHub-Event / X-Event-Type header or the body's type, event or action field. Empty: any.</div>
          <label class="rt-label">Filters (optional)</label>
          {filters.map((f, i) => (
            <div class="rt-filter">
              <input class="rt-input" value={f.path} placeholder="ref" onInput={(e) => setFilters(filters.map((x, j) => (j === i ? { ...x, path: (e.target as HTMLInputElement).value } : x)))} />
              <span>=</span>
              <input class="rt-input" value={f.value} placeholder="refs/heads/main" onInput={(e) => setFilters(filters.map((x, j) => (j === i ? { ...x, value: (e.target as HTMLInputElement).value } : x)))} />
              <button class="sh-icon-btn" onClick={() => setFilters(filters.filter((_, j) => j !== i))}>
                <Icon name="x" size={14} />
              </button>
            </div>
          ))}
          <button type="button" class="rt-link" onClick={() => setFilters([...filters, { path: "", value: "" }])}>
            + Add a filter
          </button>
        </div>
      )}
      <div class="rt-actions">
        <button class="sh-btn primary small" disabled={busy || !name.trim() || !instruction.trim()} onClick={save}>
          {saved ? "Save" : "Create"}
        </button>
        {saved && (
          <button
            class="sh-btn small"
            onClick={async () => {
              try {
                await api.runRoutine(saved.id);
                setNote("Started a test run");
                loadRuns(saved.id);
              } catch (e) {
                setErr((e as Error).message);
              }
            }}
          >
            <Icon name="play" size={13} /> Run now
          </button>
        )}
        <span style={{ flex: 1 }} />
        {note && <span class="rt-note">{note}</span>}
        {saved && (
          <button
            class="sh-btn small"
            onClick={async () => {
              if (!confirm(`Delete the routine ${saved.name}?`)) return;
              await api.deleteRoutine(saved.id);
              onDone();
            }}
          >
            Delete
          </button>
        )}
      </div>
      {saved && (
        <>
          <label class="rt-label">Runs</label>
          {runs.length === 0 && <div class="rt-hint">No runs yet.</div>}
          {runs.map((run) => (
            <div class="rt-run">
              <RunBadge run={run} />
              <span class="t">{shortTime(run.scheduled_for ?? run.started_at)}</span>
              <span class="src">{run.source === "test" ? "test" : run.source}</span>
              {run.error && <span class="err">{run.error}</span>}
            </div>
          ))}
        </>
      )}
    </div>
  );
}

// ---- the Routines page ----

function startOfWeek(d: Date): Date {
  const s = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  s.setDate(s.getDate() - ((s.getDay() + 6) % 7)); // Monday
  return s;
}

export function RoutinesPage({ sessions, chats }: { sessions: Session[]; chats: Chat[] }) {
  const [week, setWeek] = useState(() => startOfWeek(new Date()));
  const [routines, setRoutines] = useState<Routine[] | null>(null);
  const [entries, setEntries] = useState<CalendarEntry[]>([]);
  const [editing, setEditing] = useState<{ routine: Routine | null; target: { session_id?: string; chat_id?: string } } | null>(null);
  const [picking, setPicking] = useState(false);
  const end = useMemo(() => new Date(week.getTime() + 7 * 86400000), [week]);
  const load = () => {
    api.routines().then((r) => setRoutines(r.routines));
    api.routineCalendar(week, end).then((r) => setEntries(r.entries));
  };
  useEffect(load, [week]);
  const byId = useMemo(() => new Map((routines ?? []).map((r) => [r.id, r])), [routines]);
  const targetName = (r: Routine) => (r.session_id ? sessions.find((s) => s.id === r.session_id)?.name : chats.find((c) => c.id === r.chat_id)?.name) ?? "?";
  const days = Array.from({ length: 7 }, (_, i) => new Date(week.getTime() + i * 86400000));
  const today = new Date().toDateString();
  const upcoming = entries.filter((e) => !e.run && new Date(e.at) > new Date()).sort((a, b) => a.at.localeCompare(b.at)).slice(0, 8);
  const agentTargets = sessions.filter((s) => s.interface === "conversation");

  if (editing)
    return (
      <div class="rt-page">
        <div class="rt-page-editor">
          <RoutineEditor
            routine={editing.routine}
            target={editing.target}
            onDone={() => {
              setEditing(null);
              load();
            }}
          />
        </div>
      </div>
    );

  return (
    <div class="rt-page">
      <div class="rt-page-head">
        <h2>Routines</h2>
        <span style={{ flex: 1 }} />
        <span class="rt-hint">Times in {localTZ}</span>
        <div class="sh-menu-wrap">
          <button class="sh-btn small primary" onClick={() => setPicking((p) => !p)}>
            <Icon name="plus" size={14} /> New routine
          </button>
          {picking && (
            <div class="sh-menu">
              {agentTargets.map((s) => (
                <button
                  onClick={() => {
                    setPicking(false);
                    setEditing({ routine: null, target: { session_id: s.id } });
                  }}
                >
                  <Avatar id={s.id} size={18} /> {s.name}
                </button>
              ))}
              {chats.length > 0 && <hr />}
              {chats.map((c) => (
                <button
                  onClick={() => {
                    setPicking(false);
                    setEditing({ routine: null, target: { chat_id: c.id } });
                  }}
                >
                  <Icon name="chats" size={16} /> {c.name}
                </button>
              ))}
              {agentTargets.length === 0 && chats.length === 0 && <div class="rt-hint" style={{ padding: 8 }}>Create an agent first.</div>}
            </div>
          )}
        </div>
      </div>
      <div class="rt-week-nav">
        <button class="sh-icon-btn" onClick={() => setWeek(new Date(week.getTime() - 7 * 86400000))}>
          <Icon name="chevronLeft" size={17} />
        </button>
        <button class="sh-btn small" onClick={() => setWeek(startOfWeek(new Date()))}>
          Today
        </button>
        <button class="sh-icon-btn" onClick={() => setWeek(new Date(week.getTime() + 7 * 86400000))}>
          <Icon name="chevronRight" size={17} />
        </button>
        <b>
          {week.toLocaleDateString([], { month: "short", day: "numeric" })} – {new Date(end.getTime() - 86400000).toLocaleDateString([], { month: "short", day: "numeric", year: "numeric" })}
        </b>
      </div>
      <div class="rt-cal">
        <div class="rt-days-grid">
          {days.map((d) => {
            const list = entries.filter((e) => new Date(e.at).toDateString() === d.toDateString()).sort((a, b) => a.at.localeCompare(b.at));
            return (
              <div class={`rt-day ${d.toDateString() === today ? "today" : ""}`}>
                <div class="rt-day-head">
                  {d.toLocaleDateString([], { weekday: "short" })} <b>{d.getDate()}</b>
                </div>
                {list.slice(0, 6).map((e) => {
                  const r = byId.get(e.routine_id);
                  const st = e.run?.status;
                  return (
                    <button class={`rt-slot ${st === "failed" ? "bad" : ""}`} onClick={() => r && setEditing({ routine: r, target: {} })}>
                      <span class="time">
                        {st ? <Icon name={st === "failed" ? "x" : "check"} size={11} /> : <span class="dot" />}
                        {new Date(e.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
                      </span>
                      <span class="name">{r?.name ?? "deleted"}</span>
                      {r?.session_id && <Avatar id={r.session_id} size={16} />}
                    </button>
                  );
                })}
                {list.length > 6 && <div class="rt-more">+{list.length - 6} more</div>}
              </div>
            );
          })}
        </div>
        <div class="rt-upcoming">
          <div class="rt-label" style={{ marginTop: 0 }}>
            Upcoming runs
          </div>
          {upcoming.length === 0 && <div class="rt-hint">Nothing scheduled this week.</div>}
          {upcoming.map((e) => {
            const r = byId.get(e.routine_id);
            return (
              <div class="rt-up">
                <span class="dot" />
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div class="rt-row-name">{r?.name}</div>
                  <div class="rt-row-sub">
                    {new Date(e.at).toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit" })} · {r ? targetName(r) : ""}
                  </div>
                </div>
                {r?.session_id && <Avatar id={r.session_id} size={22} />}
              </div>
            );
          })}
        </div>
      </div>
      <div class="rt-all">
        <div class="rt-label">All routines</div>
        {routines?.length === 0 && <div class="rt-hint">No routines yet.</div>}
        {routines?.map((r) => (
          <div class="rt-all-row">
            <span class="rt-target">
              {r.session_id ? <Avatar id={r.session_id} size={20} /> : <Icon name="chats" size={16} />}
              <Link href={r.session_id ? `/sessions/${r.session_id}` : `/chats/${r.chat_id}`}>{targetName(r)}</Link>
            </span>
            <RoutineRow r={r} onOpen={() => setEditing({ routine: r, target: {} })} onChanged={load} />
          </div>
        ))}
      </div>
    </div>
  );
}
