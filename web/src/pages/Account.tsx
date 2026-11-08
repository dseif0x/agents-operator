import { useEffect, useState } from "preact/hooks";
import { api, type Account as AccountInfo, type AccountInput, type Profile, type Skill, type User } from "../api";
import { Nav } from "../components/Nav";
import { NotificationsCard } from "../components/Notifications";
import { timeAgo } from "../util";

/** What each account kind is, which fields it has and how sessions use it. */
const KINDS: Record<
  string,
  { label: string; help: string; git?: boolean; baseURL?: "optional" | "required"; models?: boolean; username?: boolean; multiline?: boolean; login?: boolean; placeholder?: string; mcp?: boolean }
> = {
  anthropic_api_key: {
    label: "Anthropic API key",
    help: "Claude Code sessions that use it get ANTHROPIC_API_KEY (and ANTHROPIC_BASE_URL); OpenCode gets it as a provider.",
    baseURL: "optional",
    placeholder: "sk-ant-api…",
  },
  claude_oauth_token: {
    label: "Claude Code token",
    help: "A long-lived token for a Claude subscription (CLAUDE_CODE_OAUTH_TOKEN). Run `claude setup-token` in any session, log in through the printed link and paste the token (sk-ant-oat…). On a narrow screen it spans several lines; copy all of them, line breaks and spaces are removed. It never rotates, so any number of sessions can use it at once.",
    placeholder: "sk-ant-oat…",
  },
  claude_login: {
    label: "Claude Code login",
    help: "Claude Code's login files, saved earlier from a session. Claude Code refreshes its tokens every few hours and the old pair stops working, so the hub copies the fresh files back from the session using the login. Sessions running at the same time on one login can sign each other out; a token avoids that.",
    login: true,
  },
  openai_api_key: { label: "OpenAI API key", help: "Codex sessions that use it get OPENAI_API_KEY; OpenCode gets it as a provider.", placeholder: "sk-…" },
  codex_login: { label: "Codex login", help: "Codex's login file, saved earlier from a session.", login: true },
  openai_compatible: {
    label: "OpenAI-compatible endpoint",
    help: "Any OpenAI-compatible API (LiteLLM, vLLM, llama.cpp, OpenRouter…). OpenCode gets it as a provider named after the account, with the models below or, if none are listed, the endpoint's own list, and the context sizes the endpoint reports (LiteLLM: max_input_tokens in its model_info).",
    baseURL: "required",
    models: true,
  },
  unjailed: {
    label: "Unjailed",
    help: "A key from unjailedai.com/dashboard/api. OpenCode gets its models with their context sizes; the first Unjailed account is the default model's provider.",
    baseURL: "optional",
  },
  git_https: {
    label: "HTTPS token",
    help: "Answers git's HTTPS logins for the host from the environment; never written to disk. The github.com token also signs in gh (GH_TOKEN) and fills the repository picker.",
    git: true,
    username: true,
  },
  mcp_server: {
    label: "MCP server",
    help: "A remote MCP server (streamable HTTP) with a bearer token, such as Home Assistant's Model Context Protocol Server integration and a long-lived access token. Sessions that pick it in their settings get its tools in Claude Code, Codex and OpenCode; the token stays in an environment variable. A server on your network needs Local network access to its address in the session.",
    baseURL: "required",
    mcp: true,
    placeholder: "Long-lived access token",
  },
  git_ssh: {
    label: "SSH key",
    help: "Written to the session (0600) with an ssh config entry for the host, which git uses.",
    git: true,
    multiline: true,
    placeholder: "-----BEGIN OPENSSH PRIVATE KEY-----",
  },
};

const AI_KINDS = ["anthropic_api_key", "claude_oauth_token", "openai_api_key", "openai_compatible", "unjailed"];
const GIT_KINDS = ["git_https", "git_ssh"];
const MCP_KINDS = ["mcp_server"];

type Group = "ai" | "git" | "mcp";

/** The agents that sign in with one account, and the kinds each can use. */
const SIGN_IN: { agent: string; label: string; kinds: string[] }[] = [
  { agent: "claude", label: "Claude Code", kinds: ["anthropic_api_key", "claude_oauth_token", "claude_login"] },
  { agent: "codex", label: "Codex", kinds: ["openai_api_key", "codex_login"] },
];

function hostLabel(host?: string) {
  return host === "*" ? "any other host" : host;
}

export function Account(props: { user: User; onLogout: () => void; embedded?: boolean; }) {
  const [accounts, setAccounts] = useState<AccountInfo[]>([]);
  const [profile, setProfile] = useState<Profile>({ git_name: "", git_email: "", defaults: {} });
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // The form open: a new account of a group, or an existing one.
  const [editing, setEditing] = useState<null | { group: Group; account?: AccountInfo }>(null);

  useEffect(() => {
    Promise.all([api.accounts(), api.profile()])
      .then(([a, p]) => {
        setAccounts(a.accounts);
        setProfile(p);
      })
      .catch((e) => setError((e as Error).message));
  }, []);

  const flash = (msg: string) => {
    setNotice(msg);
    setTimeout(() => setNotice(""), 4000);
  };

  const remove = async (a: AccountInfo) => {
    if (!confirm(`Delete ${a.name}? Sessions using it fall back to your default when they next start.`)) return;
    setError("");
    try {
      setAccounts((await api.deleteAccount(a.id)).accounts);
      setProfile(await api.profile());
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const saveProfile = async (p: Profile, msg: string) => {
    setError("");
    try {
      setProfile(await api.putProfile(p));
      flash(msg);
    } catch (err) {
      setError((err as Error).message);
    }
  };

  const ai = accounts.filter((a) => !GIT_KINDS.includes(a.kind) && !MCP_KINDS.includes(a.kind));
  const mcp = accounts.filter((a) => MCP_KINDS.includes(a.kind));
  const git = accounts.filter((a) => GIT_KINDS.includes(a.kind));
  const defaultOf = (agent: string) => {
    const s = SIGN_IN.find((x) => x.agent === agent)!;
    return ai.find((a) => a.id === profile.defaults[agent]) ?? ai.find((a) => s.kinds.includes(a.kind));
  };

  const row = (a: AccountInfo) => {
    const k = KINDS[a.kind];
    const defaults = SIGN_IN.filter((s) => defaultOf(s.agent)?.id === a.id).map((s) => s.label);
    return (
      <div class="account-row">
        <div class="account-main">
          <b>{k?.git ? hostLabel(a.host) : a.name}</b>
          <span class="muted"> · {k?.label ?? a.kind}</span>
          {defaults.length > 0 && <span class="pill">default for {defaults.join(" and ")}</span>}
          <div class="muted account-sub">
            {a.base_url && <>{a.base_url} · </>}
            {a.username && <>user {a.username} · </>}
            {a.models.length > 0 && <>{a.models.length} models · </>}
            {a.has_value ? <>changed {timeAgo(a.updated_at)}</> : <span class="error-text">value missing; sessions skip it</span>}
          </div>
        </div>
        <div class="account-actions">
          <button type="button" class="btn small" onClick={() => setEditing({ group: k?.git ? "git" : k?.mcp ? "mcp" : "ai", account: a })}>
            Edit
          </button>
          <button type="button" class="btn small danger" onClick={() => remove(a)}>
            Delete
          </button>
        </div>
      </div>
    );
  };

  const form = (group: Group) =>
    editing?.group === group && (
      <AccountForm
        key={editing.account?.id ?? "new-" + group}
        kinds={group === "ai" ? AI_KINDS : group === "git" ? GIT_KINDS : MCP_KINDS}
        account={editing.account}
        onCancel={() => setEditing(null)}
        onSaved={(list, msg) => {
          setAccounts(list);
          setEditing(null);
          flash(msg);
        }}
      />
    );

  return (
    <div class={`page ${props.embedded ? "embedded" : ""}`}>
      {!props.embedded && <Nav user={props.user} onLogout={props.onLogout} />}
      <div style="max-width:760px">
        {error && <div class="error">{error}</div>}
        {notice && <div style="color:var(--ok);margin:10px 0;font-size:14px">{notice}</div>}

        <section class="card">
          <div class="row" style="justify-content:space-between;align-items:center">
            <h3 style="margin:0">AI accounts</h3>
            {editing?.group !== "ai" && (
              <button type="button" class="btn small primary" onClick={() => setEditing({ group: "ai" })}>
                Add account
              </button>
            )}
          </div>
          <p class="muted" style="margin:6px 0 10px;font-size:13px">
            Claude Code and Codex sessions sign in with one account each: the one picked for the session, else the default below. OpenCode
            sessions get every API key and endpoint as a provider under its name; shell sessions get all of them. Values are kept in a
            Kubernetes Secret and never shown again.
          </p>
          {form("ai")}
          {ai.length === 0 && editing?.group !== "ai" && <div class="muted">No AI accounts yet.</div>}
          {ai.map(row)}
          {SIGN_IN.map((s) => {
            const options = ai.filter((a) => s.kinds.includes(a.kind));
            if (options.length < 2) return null;
            return (
              <label class="inline-select">
                Default for {s.label}
                <select
                  value={profile.defaults[s.agent] ?? ""}
                  onChange={(e) => {
                    const id = (e.target as HTMLSelectElement).value;
                    saveProfile({ ...profile, defaults: { ...profile.defaults, [s.agent]: id } }, `Default for ${s.label} saved.`);
                  }}
                >
                  <option value="">The oldest ({options[0].name})</option>
                  {options.map((a) => (
                    <option value={a.id}>{a.name}</option>
                  ))}
                </select>
              </label>
            );
          })}
        </section>

        <section class="card">
          <div class="row" style="justify-content:space-between;align-items:center">
            <h3 style="margin:0">Git accounts</h3>
            {editing?.group !== "git" && (
              <button type="button" class="btn small primary" onClick={() => setEditing({ group: "git" })}>
                Add account
              </button>
            )}
          </div>
          <p class="muted" style="margin:6px 0 10px;font-size:13px">
            One HTTPS token and one SSH key per host, like Host entries in an ssh config. Use <code>*</code> as the host for every host no
            other account names. Every session gets all of them unless its git credentials are turned off.
          </p>
          {form("git")}
          {git.length === 0 && editing?.group !== "git" && <div class="muted">No git accounts yet.</div>}
          {git.map(row)}
        </section>

        <section class="card">
          <div class="row" style="justify-content:space-between;align-items:center">
            <h3 style="margin:0">MCP servers</h3>
            {editing?.group !== "mcp" && (
              <button type="button" class="btn small primary" onClick={() => setEditing({ group: "mcp" })}>
                Add server
              </button>
            )}
          </div>
          <p class="muted" style="margin:6px 0 10px;font-size:13px">
            Tools from other services (Home Assistant, a ticket tracker…) for your agents. Pick them per agent in its settings; they apply
            when the agent restarts.
          </p>
          {form("mcp")}
          {mcp.length === 0 && editing?.group !== "mcp" && <div class="muted">No MCP servers yet.</div>}
          {mcp.map(row)}
        </section>

        <SkillsCard onError={setError} onNotice={flash} />

        <IdentityForm profile={profile} onSave={(p) => saveProfile(p, "Git identity saved.")} />
        <NotificationsCard />
      </div>
    </div>
  );
}

function AccountForm(props: {
  kinds: string[];
  account?: AccountInfo;
  onCancel: () => void;
  onSaved: (accounts: AccountInfo[], msg: string) => void;
}) {
  const a = props.account;
  const [kind, setKind] = useState(a?.kind ?? props.kinds[0]);
  const [name, setName] = useState(a?.name ?? "");
  const [host, setHost] = useState(a?.host ?? "");
  const [baseURL, setBaseURL] = useState(a?.base_url ?? "");
  const [username, setUsername] = useState(a?.username ?? "");
  const [models, setModels] = useState((a?.models ?? []).join("\n"));
  const [value, setValue] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const k = KINDS[kind];

  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const input: AccountInput = { kind, name, host, base_url: baseURL, username, value };
    if (k.models) input.models = models.split(/[\n,]/).map((m) => m.trim()).filter(Boolean);
    try {
      const r = a ? await api.updateAccount(a.id, input) : await api.createAccount(input);
      props.onSaved(r.accounts, a ? "Saved. Sessions pick up changes when they next start." : "Added. Sessions pick it up when they next start.");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="account-form" onSubmit={submit}>
      {!a && (
        <label>
          Type
          <select value={kind} onChange={(e) => setKind((e.target as HTMLSelectElement).value)}>
            {props.kinds.map((kd) => (
              <option value={kd}>{KINDS[kd].label}</option>
            ))}
          </select>
        </label>
      )}
      <div class="muted" style="font-size:12px;margin:2px 0 6px">
        {k.help}
      </div>
      {k.git ? (
        <label>
          Host
          <input value={host} onInput={(e) => setHost((e.target as HTMLInputElement).value)} placeholder="github.com, *.example.com or *" autocomplete="off" />
        </label>
      ) : (
        <label>
          Name
          <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} placeholder={k.mcp ? "Home Assistant" : "Work, Personal, LiteLLM…"} autocomplete="off" />
        </label>
      )}
      {k.baseURL && (
        <label>
          {k.mcp ? "URL" : "Base URL"}
          {k.baseURL === "optional" && " (optional)"}
          <input
            value={baseURL}
            onInput={(e) => setBaseURL((e.target as HTMLInputElement).value)}
            placeholder={
              k.mcp
                ? "http://home-assistant.home-assistant:8080/api/mcp"
                : kind === "unjailed"
                  ? "https://unjailedai.com/v1"
                  : kind === "anthropic_api_key"
                    ? "https://api.anthropic.com"
                    : "http://litellm:4000/v1"
            }
            autocomplete="off"
          />
        </label>
      )}
      {k.models && (
        <label>
          Models (optional, one per line)
          <textarea value={models} onInput={(e) => setModels((e.target as HTMLTextAreaElement).value)} rows={3} placeholder="Empty: ask the endpoint" />
        </label>
      )}
      {k.username && (
        <label>
          Username (optional)
          <input value={username} onInput={(e) => setUsername((e.target as HTMLInputElement).value)} placeholder="x-access-token" autocomplete="off" />
        </label>
      )}
      {!k.login && (
        <label>
          {k.multiline ? "Private key" : kind === "git_https" || k.mcp ? "Token" : "Key"}
          {a && " (empty: keep the current one)"}
          {k.multiline ? (
            <textarea value={value} onInput={(e) => setValue((e.target as HTMLTextAreaElement).value)} placeholder={k.placeholder} rows={4} />
          ) : (
            <input type="password" value={value} onInput={(e) => setValue((e.target as HTMLInputElement).value)} placeholder={k.placeholder} autocomplete="off" />
          )}
        </label>
      )}
      {error && <div class="error">{error}</div>}
      <div class="row" style="margin-top:10px">
        <button class="btn primary" disabled={busy}>
          {busy ? "Saving…" : a ? "Save" : "Add"}
        </button>
        <button type="button" class="btn" onClick={props.onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}

function IdentityForm(props: { profile: Profile; onSave: (p: Profile) => void }) {
  const [name, setName] = useState(props.profile.git_name);
  const [email, setEmail] = useState(props.profile.git_email);
  useEffect(() => {
    setName(props.profile.git_name);
    setEmail(props.profile.git_email);
  }, [props.profile.git_name, props.profile.git_email]);
  const changed = name !== props.profile.git_name || email !== props.profile.git_email;
  return (
    <form
      class="card"
      onSubmit={(e) => {
        e.preventDefault();
        props.onSave({ ...props.profile, git_name: name, git_email: email });
      }}
    >
      <h3 style="margin:0">Git identity</h3>
      <p class="muted" style="margin:6px 0 0;font-size:13px">
        Written to ~/.gitconfig on a session's first start.
      </p>
      <label>
        user.name
        <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} autocomplete="off" />
      </label>
      <label>
        user.email
        <input value={email} onInput={(e) => setEmail((e.target as HTMLInputElement).value)} autocomplete="off" />
      </label>
      <div class="row" style="margin-top:12px">
        <button class="btn primary" disabled={!changed}>
          Save
        </button>
      </div>
    </form>
  );
}

const SKILL_TEMPLATE = `Steps the agent follows when the skill fits, for example:

1. Use the home-assistant tools to read yesterday's energy sensors.
2. Compare with the day before.
3. Answer in three short lines: total kWh, the biggest consumer, anything unusual.`;

/** The owner's skill library: instructions an agent loads when they fit the task. */
function SkillsCard(props: { onError: (msg: string) => void; onNotice: (msg: string) => void }) {
  const [skills, setSkills] = useState<Skill[]>([]);
  const [editing, setEditing] = useState<Skill | "new" | null>(null);
  const load = () =>
    api
      .skills()
      .then((r) => setSkills(r.skills))
      .catch((e) => props.onError((e as Error).message));
  useEffect(() => {
    load();
  }, []);
  const remove = async (s: Skill) => {
    if (!confirm(`Delete the skill ${s.name}? Agents that have it lose it right away.`)) return;
    try {
      await api.deleteSkill(s.id);
      load();
    } catch (e) {
      props.onError((e as Error).message);
    }
  };
  return (
    <section class="card">
      <div class="row" style="justify-content:space-between;align-items:center">
        <h3 style="margin:0">Skills</h3>
        {editing === null && (
          <button type="button" class="btn small primary" onClick={() => setEditing("new")}>
            Add skill
          </button>
        )}
      </div>
      <p class="muted" style="margin:6px 0 10px;font-size:13px">
        Instructions an agent loads when they fit the task (Agent Skills: a SKILL.md for Claude Code, Codex and OpenCode). Pick them per
        agent in its settings. Running agents get new and changed skills right away; Claude Code picks them up mid-session, Codex and
        OpenCode when they next start.
      </p>
      {editing !== null && (
        <SkillForm
          key={editing === "new" ? "new" : editing.id}
          skill={editing === "new" ? undefined : editing}
          onCancel={() => setEditing(null)}
          onSaved={(msg) => {
            setEditing(null);
            load();
            props.onNotice(msg);
          }}
        />
      )}
      {skills.length === 0 && editing === null && <div class="muted">No skills yet.</div>}
      {skills.map((s) => (
        <div class="account-row">
          <div class="account-main">
            <b>{s.name}</b>
            <div class="muted account-sub">
              {s.description} · changed {timeAgo(s.updated_at)}
            </div>
          </div>
          <div class="account-actions">
            <button type="button" class="btn small" onClick={() => setEditing(s)}>
              Edit
            </button>
            <button type="button" class="btn small danger" onClick={() => remove(s)}>
              Delete
            </button>
          </div>
        </div>
      ))}
    </section>
  );
}

function SkillForm(props: { skill?: Skill; onCancel: () => void; onSaved: (msg: string) => void }) {
  const s = props.skill;
  const [name, setName] = useState(s?.name ?? "");
  const [description, setDescription] = useState(s?.description ?? "");
  const [instructions, setInstructions] = useState(s?.instructions ?? "");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const input = { name, description, instructions };
      if (s) await api.updateSkill(s.id, input);
      else await api.createSkill(input);
      props.onSaved(s ? "Skill saved. Running agents with it get the change right away." : "Skill added. Pick it in an agent's settings.");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <form class="account-form" onSubmit={submit}>
      <label>
        Name
        <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} placeholder="energy-report" autocomplete="off" spellcheck={false} />
      </label>
      <div class="muted" style="font-size:12px;margin:2px 0 6px">
        Lower case letters, digits and hyphens.
      </div>
      <label>
        Description
        <input
          value={description}
          onInput={(e) => setDescription((e.target as HTMLInputElement).value)}
          placeholder="Summarises yesterday's energy use from Home Assistant. Use when asked for an energy report or recap."
        />
      </label>
      <div class="muted" style="font-size:12px;margin:2px 0 6px">
        What the skill does and when to use it: the agent decides from this whether to load it.
      </div>
      <label>
        Instructions
        <textarea value={instructions} onInput={(e) => setInstructions((e.target as HTMLTextAreaElement).value)} placeholder={SKILL_TEMPLATE} rows={10} />
      </label>
      {error && <div class="error">{error}</div>}
      <div class="row" style="margin-top:10px">
        <button class="btn primary" disabled={busy}>
          {busy ? "Saving…" : s ? "Save" : "Add"}
        </button>
        <button type="button" class="btn" onClick={props.onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}
