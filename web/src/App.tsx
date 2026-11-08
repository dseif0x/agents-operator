import { onUpdate } from "./update";
import { useEffect, useState } from "preact/hooks";
import { api, ApiError, setCsrf, type User } from "./api";
import { navigate, useRoute } from "./router";
import { Login } from "./pages/Login";
import { SessionPage } from "./pages/SessionPage";
import { Shell } from "./shell/Shell";
import { lastPath, rememberPath, reportPresence } from "./pwa";

export function App() {
  const route = useRoute();
  const [user, setUser] = useState<User | null | undefined>(undefined);
  const [outdated, setOutdated] = useState(false);
  useEffect(() => onUpdate(() => setOutdated(true)), []);

  useEffect(() => {
    api
      .me()
      .then((r) => {
        setCsrf(r.csrf);
        setUser(r.user);
      })
      .catch((e) => {
        if (e instanceof ApiError && e.status === 401) setUser(null);
        else setUser(null);
      });
  }, []);

  // Remember the page for the next start, and tell the hub what is on
  // screen so it holds back notifications about it.
  useEffect(() => {
    rememberPath(location.pathname);
    if (user) reportPresence(location.pathname);
  }, [route.path, route.params.id, user]);

  useEffect(() => {
    if (user === null && route.path !== "/login") navigate("/login", true);
    if (user && route.path === "/login") navigate(lastPath(), true);
  }, [user, route.path]);

  if (user === undefined) return <div class="page muted">Loading…</div>;

  if (!user || route.path === "/login") {
    return (
      <Login
        onLogin={(u, csrf) => {
          setCsrf(csrf);
          setUser(u);
          navigate(lastPath(), true);
        }}
      />
    );
  }

  const logout = async () => {
    await api.logout().catch(() => undefined);
    setUser(null);
  };

  const banner = outdated && (
    <div class="update-banner" role="status">
      <span>A new version of agents-operator is out.</span>
      <button onClick={() => location.reload()}>Reload</button>
      <button class="x" title="Later" onClick={() => setOutdated(false)}>
        ×
      </button>
    </div>
  );
  switch (route.path) {
    case "/sessions/:id/terminal":
      return (
        <>
          {banner}
          <SessionPage id={route.params.id} />
        </>
      );
    default:
      // "/", "/sessions/:id", "/chats", "/chats/:id": the workspace.
      return (
        <>
          {banner}
          <Shell user={user} route={route} onLogout={logout} />
        </>
      );
  }
}
