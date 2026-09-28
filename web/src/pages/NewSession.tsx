import { useEffect, useState } from "preact/hooks";
import { api, type CreateSessionRequest, type User } from "../api";
import { Nav } from "../components/Nav";
import { navigate } from "../router";

export function NewSession(props: { user: User; onLogout: () => void }) {
  const [agents, setAgents] = useState<string[]>(["claude", "opencode", "codex", "shell"]);
  const [name, setName] = useState("");
  const [agent, setAgent] = useState("claude");
  const [repo, setRepo] = useState("");
  const [branch, setBranch] = useState("");
  const [imageTag, setImageTag] = useState("");
  const [pvcSize, setPvcSize] = useState("");
  const [storageClass, setStorageClass] = useState("");
  const [cpu, setCpu] = useState("");
  const [memory, setMemory] = useState("");
  const [env, setEnv] = useState("");
  const [autonomous, setAutonomous] = useState(true);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.sessions().then((r) => r.agents?.length && setAgents(r.agents)).catch(() => undefined);
  }, []);

  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    const req: CreateSessionRequest = { name, agent, repo_url: repo, branch, autonomous };
    if (imageTag) req.image_tag = imageTag;
    if (pvcSize) req.pvc_size = pvcSize;
    if (storageClass) req.storage_class = storageClass;
    if (cpu || memory) req.resources = { requests: {}, limits: { cpu: cpu || undefined, memory: memory || undefined } };
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
    try {
      const s = await api.createSession(req);
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
        <h3 style="margin:0">New session</h3>
        <div class="form-grid">
          <div>
            <label>Name</label>
            <input value={name} onInput={(e) => setName((e.target as HTMLInputElement).value)} placeholder="fix-login-bug" required autoFocus />
          </div>
          <div>
            <label>Agent</label>
            <select value={agent} onChange={(e) => setAgent((e.target as HTMLSelectElement).value)}>
              {agents.map((a) => (
                <option value={a}>{a}</option>
              ))}
            </select>
          </div>
          <div>
            <label>Repository URL</label>
            <input value={repo} onInput={(e) => setRepo((e.target as HTMLInputElement).value)} placeholder="git@github.com:you/repo.git" inputMode="url" />
          </div>
          <div>
            <label>Branch</label>
            <input value={branch} onInput={(e) => setBranch((e.target as HTMLInputElement).value)} placeholder="main" />
          </div>
        </div>
        <div class="checkbox">
          <input id="auto" type="checkbox" checked={autonomous} onChange={(e) => setAutonomous((e.target as HTMLInputElement).checked)} />
          <label for="auto" style="margin:0;color:inherit">
            Autonomous (skip permission prompts; Claude gets <code>--dangerously-skip-permissions</code>)
          </label>
        </div>
        <details>
          <summary>Advanced: storage, resources, image, environment</summary>
          <div class="form-grid">
            <div>
              <label>PVC size (default from chart)</label>
              <input value={pvcSize} onInput={(e) => setPvcSize((e.target as HTMLInputElement).value)} placeholder="20Gi" />
            </div>
            <div>
              <label>Storage class</label>
              <input value={storageClass} onInput={(e) => setStorageClass((e.target as HTMLInputElement).value)} placeholder="nfs-fast" />
            </div>
            <div>
              <label>CPU limit (can only be lowered)</label>
              <input value={cpu} onInput={(e) => setCpu((e.target as HTMLInputElement).value)} placeholder="2" />
            </div>
            <div>
              <label>Memory limit (can only be lowered)</label>
              <input value={memory} onInput={(e) => setMemory((e.target as HTMLInputElement).value)} placeholder="4Gi" />
            </div>
            <div>
              <label>Runner image tag</label>
              <input value={imageTag} onInput={(e) => setImageTag((e.target as HTMLInputElement).value)} placeholder="latest" />
            </div>
          </div>
          <label>Extra environment (KEY=value per line, not secret)</label>
          <textarea value={env} onInput={(e) => setEnv((e.target as HTMLTextAreaElement).value)} placeholder="NODE_OPTIONS=--max-old-space-size=2048" />
        </details>
        {error && <div class="error">{error}</div>}
        <div class="row" style="margin-top:16px">
          <button class="btn primary" disabled={busy}>
            {busy ? "Creating…" : "Create session"}
          </button>
          <button type="button" class="btn" onClick={() => navigate("/")}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
