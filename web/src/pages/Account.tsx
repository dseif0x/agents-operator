import { useEffect, useState } from "preact/hooks";
import { api, type Credential, type User } from "../api";
import { Nav } from "../components/Nav";
import { timeAgo } from "../util";

const FIELDS: { kind: string; label: string; help: string; multiline?: boolean; secret: boolean }[] = [
  { kind: "anthropic_api_key", label: "Anthropic API key", help: "ANTHROPIC_API_KEY for Claude Code (API key mode).", secret: true },
  { kind: "anthropic_base_url", label: "Anthropic base URL", help: "Optional ANTHROPIC_BASE_URL, e.g. an in-cluster proxy.", secret: false },
  { kind: "openai_api_key", label: "OpenAI API key", help: "OPENAI_API_KEY for Codex.", secret: true },
  { kind: "git_user_name", label: "Git user.name", help: "Seeded into ~/.gitconfig on first boot.", secret: false },
  { kind: "git_user_email", label: "Git user.email", help: "Seeded into ~/.gitconfig on first boot.", secret: false },
  { kind: "git_ssh_key", label: "Git SSH private key", help: "Installed as ~/.ssh/id_ed25519 (0600) in every session.", multiline: true, secret: true },
  { kind: "git_https_token", label: "Git HTTPS token", help: "Used for https:// clones and pushes on any host; served by a credential helper from the environment, never written to disk.", secret: true },
  { kind: "github_token", label: "GitHub token", help: "Exposed as GH_TOKEN / GITHUB_TOKEN so gh (PRs, checks, Actions runs) and https://github.com clones work. A fine-grained PAT scoped to the repos you use is enough.", secret: true },
];

const LOGIN_KINDS: Record<string, string> = {
  claude_login: "Claude Code login (subscription mode)",
  codex_login: "Codex login",
};

export function Account(props: { user: User; onLogout: () => void }) {
  const [creds, setCreds] = useState<Credential[]>([]);
  const [values, setValues] = useState<Record<string, string>>({});
  const [error, setError] = useState("");
  const [saved, setSaved] = useState("");
  const [busy, setBusy] = useState(false);

  const load = () =>
    api
      .credentials()
      .then((r) => setCreds(r.credentials))
      .catch((e) => setError((e as Error).message));

  useEffect(() => {
    load();
  }, []);

  const byKind = Object.fromEntries(creds.map((c) => [c.kind, c]));

  const save = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    setSaved("");
    try {
      const r = await api.putCredentials(values);
      setCreds(r.credentials);
      setValues({});
      setSaved("Saved. New sessions will pick these up; running ones keep what they started with.");
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: string) => {
    if (!confirm(`Remove ${kind}?`)) return;
    try {
      await api.deleteCredential(kind);
      await load();
    } catch (err) {
      setError((err as Error).message);
    }
  };

  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <form class="card" onSubmit={save} style="max-width:720px">
        <h3 style="margin:0">Credentials</h3>
        <p class="muted" style="margin:6px 0 0">
          Stored in a Kubernetes Secret for your account and projected into each session pod. Secret values are never shown again.
        </p>
        {FIELDS.map((f) => {
          const cur = byKind[f.kind];
          return (
            <div>
              <label>
                {f.label}
                {cur && (
                  <>
                    {" "}
                    · <span style="color:var(--ok)">set {timeAgo(cur.updated_at)}</span>{" "}
                    <button type="button" class="btn small danger" onClick={() => remove(f.kind)}>
                      remove
                    </button>
                  </>
                )}
              </label>
              {f.multiline ? (
                <textarea
                  value={values[f.kind] ?? ""}
                  onInput={(e) => setValues({ ...values, [f.kind]: (e.target as HTMLTextAreaElement).value })}
                  placeholder={cur ? "(set; paste to replace)" : "-----BEGIN OPENSSH PRIVATE KEY-----"}
                />
              ) : (
                <input
                  type={f.secret ? "password" : "text"}
                  value={values[f.kind] ?? (f.secret ? "" : (cur?.value ?? ""))}
                  onInput={(e) => setValues({ ...values, [f.kind]: (e.target as HTMLInputElement).value })}
                  placeholder={cur && f.secret ? "(set; type to replace)" : ""}
                  autocomplete="off"
                />
              )}
              <div class="muted" style="font-size:12px;margin-top:3px">
                {f.help}
              </div>
            </div>
          );
        })}
        <h4 style="margin:22px 0 4px">Saved logins</h4>
        <p class="muted" style="margin:0 0 6px;font-size:13px">
          Run the CLI's own login inside a session, then use “Save login to account” on the session page. Later sessions start logged in.
        </p>
        <table class="creds">
          {Object.entries(LOGIN_KINDS).map(([kind, label]) => (
            <tr>
              <td>{label}</td>
              <td>
                {byKind[kind] ? (
                  <>
                    <span style="color:var(--ok)">saved {timeAgo(byKind[kind].updated_at)}</span>{" "}
                    <button type="button" class="btn small danger" onClick={() => remove(kind)}>
                      remove
                    </button>
                  </>
                ) : (
                  <span class="muted">not saved</span>
                )}
              </td>
            </tr>
          ))}
        </table>
        {error && <div class="error">{error}</div>}
        {saved && <div style="color:var(--ok);margin:10px 0;font-size:14px">{saved}</div>}
        <div class="row" style="margin-top:16px">
          <button class="btn primary" disabled={busy || Object.keys(values).length === 0}>
            {busy ? "Saving…" : "Save"}
          </button>
        </div>
      </form>
    </div>
  );
}
