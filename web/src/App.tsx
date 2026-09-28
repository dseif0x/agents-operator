import { useEffect, useState } from "preact/hooks";
import { api, ApiError, setCsrf, type User } from "./api";
import { navigate, useRoute } from "./router";
import { Login } from "./pages/Login";
import { SessionList } from "./pages/SessionList";
import { NewSession } from "./pages/NewSession";
import { SessionPage } from "./pages/SessionPage";
import { Account } from "./pages/Account";

export function App() {
  const route = useRoute();
  const [user, setUser] = useState<User | null | undefined>(undefined);

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

  useEffect(() => {
    if (user === null && route.path !== "/login") navigate("/login", true);
    if (user && route.path === "/login") navigate("/", true);
  }, [user, route.path]);

  if (user === undefined) return <div class="page muted">Loading…</div>;

  if (!user || route.path === "/login") {
    return (
      <Login
        onLogin={(u, csrf) => {
          setCsrf(csrf);
          setUser(u);
          navigate("/", true);
        }}
      />
    );
  }

  const logout = async () => {
    await api.logout().catch(() => undefined);
    setUser(null);
  };

  switch (route.path) {
    case "/sessions/:id":
      return <SessionPage id={route.params.id} />;
    case "/new":
      return <NewSession user={user} onLogout={logout} />;
    case "/account":
      return <Account user={user} onLogout={logout} />;
    default:
      return <SessionList user={user} onLogout={logout} />;
  }
}
