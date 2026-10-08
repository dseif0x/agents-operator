import { useEffect, useMemo, useState } from "preact/hooks";
import { api, type K8sAccess, type CreateSessionRequest, type GitHubRepo, type Repo, type Session, type SessionDefaults, type Toleration, type User } from "../api";
import { Nav } from "../components/Nav";
import { navigate } from "../router";

/** Parse one repo per line: `url [branch] [path]`. */
export function parseRepos(text: string): { repos: Repo[]; error?: string } {
  const repos: Repo[] = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const parts = line.split(/\s+/);
    if (parts.length > 3) return { repos, error: `too many fields: ${line}` };
    const r: Repo = { url: parts[0], path: "" };
    if (parts[1]) r.branch = parts[1];
    if (parts[2]) r.path = parts[2];
    repos.push(r);
  }
  return { repos };
}

/** Parse `key=value` lines (a bare key means an empty value). */
export function parseKeyValues(text: string): { values: Record<string, string>; error?: string } {
  const values: Record<string, string> = {};
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const i = line.indexOf("=");
    const key = (i < 0 ? line : line.slice(0, i)).trim();
    if (!key) return { values, error: `missing key: ${line}` };
    values[key] = i < 0 ? "" : line.slice(i + 1).trim();
  }
  return { values };
}

/**
 * Parse one toleration per line: `key=value:Effect`, `key:Effect`, `key`
 * or `*` (tolerate every taint). A value means operator Equal, no value
 * means Exists. `:NoExecute/30` limits how long the pod stays after the
 * taint appears.
 */
export function parseTolerations(text: string): { tolerations: Toleration[]; error?: string } {
  const tolerations: Toleration[] = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const m = /^([^=:\s]*)(?:=([^:\s]*))?(?::([A-Za-z]+)(?:\/(\d+))?)?$/.exec(line);
    if (!m) return { tolerations, error: `cannot parse toleration: ${line}` };
    const [, key, value, effect, seconds] = m;
    const t: Toleration = {};
    if (key && key !== "*") t.key = key;
    if (value !== undefined) {
      t.operator = "Equal";
      t.value = value;
    } else {
      t.operator = "Exists";
    }
    if (effect) t.effect = effect as Toleration["effect"];
    if (seconds !== undefined) t.tolerationSeconds = Number(seconds);
    tolerations.push(t);
  }
  return { tolerations };
}

/** The line form of a toleration, inverse of parseTolerations. */
export function formatToleration(t: Toleration): string {
  return `${t.key || "*"}${t.operator === "Equal" ? "=" + (t.value ?? "") : ""}${t.effect ? ":" + t.effect : ""}${
    t.tolerationSeconds !== undefined ? "/" + t.tolerationSeconds : ""
  }`;
}

const keyValueLines = (m: Record<string, string | undefined> | undefined, skip: string[] = []) =>
  Object.entries(m || {})
    .filter(([k, v]) => !skip.includes(k) && v !== undefined)
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");

// NewSession creates a session, or with `edit` set (a session id) replaces
// the settings of a stopped one: same form, the fields that shape the
// volume (agent, PVC size, storage class) locked.
export function NewSession(props: { user: User; onLogout: () => void; edit?: string }) {
  const editing = !!props.edit;
  const [agents, setAgents] = useState<string[]>(["claude", "opencode", "codex", "shell"]);
  const [name, setName] = useState("");
  const [agent, setAgent] = useState("claude");
  const [repos, setRepos] = useState("");
  const [imageTag, setImageTag] = useState("");
  const [pvcSize, setPvcSize] = useState("");
  const [storageClass, setStorageClass] = useState("");
  const [cpu, setCpu] = useState("");
  const [memory, setMemory] = useState("");
  const [env, setEnv] = useState("");
  const [runtimeClass, setRuntimeClass] = useState("");
  const [nodeSelector, setNodeSelector] = useState("");
  const [tolerations, setTolerations] = useState("");
  const [extended, setExtended] = useState("");
  const [k8sAccess, setK8sAccess] = useState<K8sAccess>("off");
  const [k8sNamespaces, setK8sNamespaces] = useState("");
  const [autonomous, setAutonomous] = useState(true);
  const [editable, setEditable] = useState<boolean | null>(editing ? null : true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [defaults, setDefaults] = useState<SessionDefaults | null>(null);
  const [gh, setGh] = useState<{ configured: boolean; repos: GitHubRepo[]; error?: string } | null>(null);
  const [ghLoading, setGhLoading] = useState(false);
  const [query, setQuery] = useState("");

  const loadGitHub = (refresh = false) => {
    setGhLoading(true);
    api
      .githubRepos(refresh)
      .then(setGh)
      .catch((e) => setGh({ configured: true, repos: [], error: (e as Error).message }))
      .finally(() => setGhLoading(false));
  };

  useEffect(() => {
    api
      .sessions()
      .then((r) => {
        if (r.agents?.length) setAgents(r.agents);
        if (r.defaults) setDefaults(r.defaults);
      })
      .catch(() => undefined);
    loadGitHub();
  }, []);

  useEffect(() => {
    if (!props.edit) return;
    api
      .session(props.edit)
      .then((s: Session) => {
        setName(s.name);
        setAgent(s.agent);
        setRepos(s.repos.map((r) => [r.url, r.branch, r.path].filter(Boolean).join(" ")).join("\n"));
        setImageTag(s.image_tag);
        setPvcSize(s.pvc_size);
        setStorageClass(s.storage_class);
        setCpu(s.resources?.limits?.cpu || "");
        setMemory(s.resources?.limits?.memory || "");
        setExtended(keyValueLines(s.resources?.limits, ["cpu", "memory"]));
        setEnv(keyValueLines(s.env));
        setRuntimeClass(s.runtime_class || "");
        setNodeSelector(keyValueLines(s.node_selector));
        setTolerations((s.tolerations || []).map(formatToleration).join("\n"));
        setK8sAccess(s.k8s_access || "off");
        setK8sNamespaces((s.k8s_namespaces || []).join(", "));
        setAutonomous(s.autonomous);
        setEditable(s.state === "stopped" || s.state === "failed");
      })
      .catch((e) => setError((e as Error).message));
  }, [props.edit]);

  // Search across name and description; the server already sorted by usage, then name.
  const filtered = useMemo(() => {
    const list = gh?.repos ?? [];
    const q = query.trim().toLowerCase();
    const terms = q ? q.split(/\s+/) : [];
    return list.filter((r) => terms.every((t) => r.full_name.toLowerCase().includes(t) || (r.description || "").toLowerCase().includes(t))).slice(0, 60);
  }, [gh, query]);

  const selectedUrls = useMemo(() => new Set(parseRepos(repos).repos.map((r) => r.url.toLowerCase())), [repos]);

  const addRepo = (r: GitHubRepo) => {
    const line = `${r.clone_url} ${r.default_branch}`;
    setRepos((cur) => (cur.trim() ? cur.replace(/\s*$/, "") + "\n" + line + "\n" : line + "\n"));
    if (!name) setName(r.full_name.split("/")[1] ?? "");
  };

  // "default 2 · up to 8": what a session gets without asking, and the most it may ask for.
  const range = (name: "cpu" | "memory") => {
    const d = defaults?.resources?.limits?.[name];
    const m = defaults?.max_resources?.limits?.[name] || d;
    if (!d) return "";
    return m && m !== d ? `default ${d} · up to ${m}` : `default ${d}, the maximum`;
  };
  const extendedCaps = Object.entries(defaults?.max_resources?.limits || {})
    .filter(([k, v]) => k !== "cpu" && k !== "memory" && v)
    .map(([k, v]) => `${k} up to ${v}`)
    .join(", ");

  const submit = async (e: Event) => {
    e.preventDefault();
    setError("");
    const parsed = parseRepos(repos);
    if (parsed.error) {
      setError(parsed.error);
      return;
    }
    setBusy(true);
    const req: CreateSessionRequest = { name, agent, repos: parsed.repos, autonomous, k8s_access: k8sAccess };
    if (k8sAccess === "namespace") {
      req.k8s_namespaces = k8sNamespaces
        .split(/[\s,]+/)
        .map((ns) => ns.trim())
        .filter(Boolean);
      if (!req.k8s_namespaces.length) {
        setError("namespace access needs at least one namespace");
        setBusy(false);
        return;
      }
    }
    if (imageTag) req.image_tag = imageTag;
    if (pvcSize) req.pvc_size = pvcSize;
    if (storageClass) req.storage_class = storageClass;
    const ext = parseKeyValues(extended);
    if (ext.error) {
      setError(ext.error);
      setBusy(false);
      return;
    }
    if (cpu || memory || Object.keys(ext.values).length) {
      req.resources = { requests: {}, limits: { ...ext.values, cpu: cpu || undefined, memory: memory || undefined } };
    }
    const envMap: Record<string, string> = {};
    for (const line of env.split("\n")) {
      const t = line.trim();
      if (!t || t.startsWith("#")) continue;
      const i = t.indexOf("=");
      if (i <= 0) {
        setError(`bad env line: ${t}`);
        setBusy(false);
        return;
      }
      envMap[t.slice(0, i).trim()] = t.slice(i + 1);
    }
    if (Object.keys(envMap).length) req.env = envMap;
    if (runtimeClass.trim()) req.runtime_class = runtimeClass.trim();
    const sel = parseKeyValues(nodeSelector);
    const tol = parseTolerations(tolerations);
    if (sel.error || tol.error) {
      setError(sel.error || tol.error || "");
      setBusy(false);
      return;
    }
    if (Object.keys(sel.values).length) req.node_selector = sel.values;
    if (tol.tolerations.length) req.tolerations = tol.tolerations;
    try {
      const s = props.edit ? await api.updateSession(props.edit, req) : await api.createSession(req);
      navigate(`/sessions/${s.id}`);
    } catch (err) {
      setError((err as Error).message);
      setBusy(false);
    }
  };

  return (
    <div class="page">
      <Nav user={props.user} onLogout={props.onLogout} />
      <form class="card" onSubmit={submit} style="max-width:720px">
        <h3 style="margin:0">{editing ? "Edit session" : "New session"}</h3>
        {editing && editable === false && <div class="banner error" style="position:static;margin-top:8px">Stop the session to change its settings; they apply to the next start.</div>}
        {editing && editable && (
          <p class="muted" style="margin:6px 0 0;font-size:13px">
            Changes apply when the session starts again. The agent, PVC size and storage class are fixed: they shape the volume and the CLI state on it. Repositories
            added here are cloned on the next start; removed ones stay on disk.
          </p>
        )}
        <div class="form-grid">
          <div>
            <label>Name</label>
            <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} placeholder="fix-login-bug" required autoFocus />
          </div>
          <div>
            <label>Agent</label>
            <select value={agent} onChange={(e) => setAgent((e.target as HTMLSelectElement).value)} disabled={editing}>
              {agents.map((a) => (
                <option value={a}>{a}</option>
              ))}
            </select>
          </div>
        </div>
        {gh?.configured && (
          <div class="repo-browser">
            <label style="margin-top:14px">Browse your GitHub repositories</label>
            <div class="row">
              <input
                value={query}
                onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
                placeholder="Search by name or description…"
                style="flex:1"
              />
              <button type="button" class="btn small" onClick={() => loadGitHub(true)} disabled={ghLoading} title="Reload from GitHub">
                {ghLoading ? "…" : "↻"}
              </button>
            </div>
            {gh.error && <div class="error">{gh.error}</div>}
            <ul class="repo-list">
              {filtered.map((r) => {
                const selected = selectedUrls.has(r.clone_url.toLowerCase()) || selectedUrls.has(r.ssh_url?.toLowerCase());
                return (
                  <li key={r.full_name}>
                    <button type="button" class={`repo-row ${selected ? "selected" : ""}`} onClick={() => !selected && addRepo(r)} disabled={selected}>
                      <span class="repo-name">
                        {r.full_name}
                        {r.private && <span class="badge">private</span>}
                        {r.archived && <span class="badge">archived</span>}
                        {r.uses > 0 && <span class="badge agent">used {r.uses}×</span>}
                      </span>
                      {r.description && <span class="repo-desc">{r.description}</span>}
                    </button>
                  </li>
                );
              })}
              {!ghLoading && filtered.length === 0 && <li class="muted" style="padding:8px">No repositories match.</li>}
              {ghLoading && gh.repos.length === 0 && <li class="muted" style="padding:8px">Loading repositories…</li>}
            </ul>
          </div>
        )}
        <label>Repositories (one per line: url, optional branch, optional directory name)</label>
        <textarea
          value={repos}
          onInput={(e) => setRepos((e.target as HTMLTextAreaElement).value)}
          placeholder={"git@github.com:you/app.git main\nhttps://github.com/you/shared-lib.git\ngit@github.com:you/infra.git main infra-repo"}
          spellcheck={false}
        />
        <div class="muted" style="font-size:12px;margin-top:3px">
          Each repo is cloned into <code>/workspace/&lt;directory&gt;</code> (default: the repo name). The agent starts in <code>/workspace</code> next to an
          AGENTS.md that lists them. Leave empty for a blank workspace.
        </div>
        <div class="checkbox">
          <input id="auto" type="checkbox" checked={autonomous} onChange={(e) => setAutonomous((e.target as HTMLInputElement).checked)} />
          <label for="auto" style="margin:0;color:inherit">
            Autonomous (skip permission prompts; Claude gets <code>--dangerously-skip-permissions</code>)
          </label>
        </div>
        {defaults?.service_account && (
          <div class="form-grid">
            <div>
              <label for="k8s">Kubernetes access</label>
              <select id="k8s" value={k8sAccess} onChange={(e) => setK8sAccess((e.target as HTMLSelectElement).value as K8sAccess)}>
                <option value="off">Off</option>
                <option value="readonly">Read-only</option>
                {defaults.k8s_namespace_write && <option value="namespace">Read-only + write in namespaces</option>}
              </select>
              <div class="muted" style="font-size:12px;margin-top:3px">
                {k8sAccess === "off" && "No cluster credentials in the pod."}
                {k8sAccess === "readonly" && (
                  <>
                    Mounts the read-only ServiceAccount <code>{defaults.service_account}</code>: <code>kubectl get</code>, <code>describe</code>, <code>logs</code>;
                    no Secrets, no changes.
                  </>
                )}
                {k8sAccess === "namespace" && "The session gets a ServiceAccount of its own: read-only everywhere the shared one may read, plus the edit role in the namespaces below."}
              </div>
            </div>
            {k8sAccess === "namespace" && (
              <div>
                <label for="k8sns">Namespaces with write access</label>
                <input id="k8sns" value={k8sNamespaces} onInput={(e) => setK8sNamespaces((e.target as HTMLInputElement).value)} placeholder="dev, staging" spellcheck={false} />
                <div class="muted" style="font-size:12px;margin-top:3px">
                  Comma or space separated. The hub's own namespace and <code>kube-system</code> are refused; the namespaces must exist.
                </div>
              </div>
            )}
          </div>
        )}
        <details>
          <summary>Advanced: storage, resources, image, environment</summary>
          <div class="form-grid">
            <div>
              <label>PVC size</label>
              <input value={pvcSize} onInput={(e) => setPvcSize((e.target as HTMLInputElement).value)} placeholder={defaults?.pvc_size || "20Gi"} disabled={editing} />
            </div>
            <div>
              <label>Storage class</label>
              <input
                value={storageClass}
                onInput={(e) => setStorageClass((e.target as HTMLInputElement).value)}
                placeholder={defaults?.storage_class || "cluster default"}
                disabled={editing}
              />
            </div>
            <div>
              <label>CPU limit {range("cpu") && <span class="muted">({range("cpu")})</span>}</label>
              <input value={cpu} onInput={(e) => setCpu((e.target as HTMLInputElement).value)} placeholder={defaults?.resources?.limits?.cpu || "2"} />
            </div>
            <div>
              <label>Memory limit {range("memory") && <span class="muted">({range("memory")})</span>}</label>
              <input value={memory} onInput={(e) => setMemory((e.target as HTMLInputElement).value)} placeholder={defaults?.resources?.limits?.memory || "4Gi"} />
            </div>
            <div>
              <label>Runner image tag</label>
              <input value={imageTag} onInput={(e) => setImageTag((e.target as HTMLInputElement).value)} placeholder={defaults?.image_tag || "chart appVersion"} />
            </div>
          </div>
          <label>Extra environment (KEY=value per line, not secret)</label>
          <textarea value={env} onInput={(e) => setEnv((e.target as HTMLTextAreaElement).value)} placeholder="NODE_OPTIONS=--max-old-space-size=2048" />
        </details>
        <details>
          <summary>Advanced: scheduling (runtime class, node selector, tolerations)</summary>
          <div class="form-grid">
            <div>
              <label>Runtime class</label>
              <input
                value={runtimeClass}
                onInput={(e) => setRuntimeClass((e.target as HTMLInputElement).value)}
                placeholder={defaults?.runtime_class || "node default"}
              />
            </div>
          </div>
          <label>Extended resources (name=amount per line; whole numbers, request equals limit)</label>
          <textarea
            value={extended}
            onInput={(e) => setExtended((e.target as HTMLTextAreaElement).value)}
            placeholder={"nvidia.com/gpu=1\nhugepages-2Mi=64Mi"}
            spellcheck={false}
          />
          <div class="muted" style="font-size:12px;margin-top:3px">
            {extendedCaps ? `Limits: ${extendedCaps}. ` : ""}A GPU usually needs the matching toleration below.
          </div>
          <label>Node selector (key=value per line; added to the chart's)</label>
          <textarea
            value={nodeSelector}
            onInput={(e) => setNodeSelector((e.target as HTMLTextAreaElement).value)}
            placeholder={"kubernetes.io/arch=arm64\nnode-role.kubernetes.io/agents=true"}
            spellcheck={false}
          />
          <label>Tolerations (one per line; added to the chart's)</label>
          <textarea
            value={tolerations}
            onInput={(e) => setTolerations((e.target as HTMLTextAreaElement).value)}
            placeholder={"nvidia.com/gpu:NoSchedule\nagents=true:NoSchedule\nnode.kubernetes.io/unreachable:NoExecute/300"}
            spellcheck={false}
          />
          <div class="muted" style="font-size:12px;margin-top:3px">
            <code>key=value:Effect</code> tolerates a taint with that value (operator Equal); <code>key:Effect</code> or a bare <code>key</code> tolerates any value
            (Exists); <code>*</code> tolerates every taint. Effects: NoSchedule, PreferNoSchedule, NoExecute; <code>:NoExecute/300</code> evicts after 300 s. Taint
            the node itself with <code>kubectl taint nodes &lt;node&gt; agents=true:NoSchedule</code>.
          </div>
        </details>
        {error && <div class="error">{error}</div>}
        <div class="row" style="margin-top:16px">
          <button class="btn primary" disabled={busy || editable === false || editable === null}>
            {busy ? (editing ? "Saving…" : "Creating…") : editing ? "Save settings" : "Create session"}
          </button>
          <button type="button" class="btn" onClick={() => navigate(props.edit ? `/sessions/${props.edit}` : "/")}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
