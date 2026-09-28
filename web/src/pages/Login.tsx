import { useState } from "preact/hooks";
import { api, type User } from "../api";

export function Login(props: { onLogin: (u: User, csrf: string) => void }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const r = await api.login(username, password);
      props.onLogin(r.user, r.csrf);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="page">
      <form class="card login" onSubmit={submit}>
        <h2 style="margin:0 0 6px">agents-operator</h2>
        <p class="muted" style="margin:0 0 10px">
          Mission control for your coding agents.
        </p>
        <label>Username</label>
        <input value={username} onInput={(e) => setUsername((e.target as HTMLInputElement).value)} autocomplete="username" autoFocus />
        <label>Password</label>
        <input
          type="password"
          value={password}
          onInput={(e) => setPassword((e.target as HTMLInputElement).value)}
          autocomplete="current-password"
        />
        {error && <div class="error">{error}</div>}
        <button class="btn primary" style="margin-top:16px;width:100%;justify-content:center" disabled={busy}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}
