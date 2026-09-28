// A very small path router: the URL is the state.
import { useEffect, useState } from "preact/hooks";

export interface Route {
  path: string;
  params: Record<string, string>;
}

const listeners = new Set<() => void>();

export function navigate(path: string, replace = false) {
  if (replace) history.replaceState(null, "", path);
  else history.pushState(null, "", path);
  listeners.forEach((l) => l());
}

export function useRoute(): Route {
  const [path, setPath] = useState(location.pathname);
  useEffect(() => {
    const update = () => setPath(location.pathname);
    listeners.add(update);
    addEventListener("popstate", update);
    return () => {
      listeners.delete(update);
      removeEventListener("popstate", update);
    };
  }, []);
  return match(path);
}

function match(path: string): Route {
  const parts = path.split("/").filter(Boolean);
  if (parts.length === 0) return { path: "/", params: {} };
  if (parts[0] === "login") return { path: "/login", params: {} };
  if (parts[0] === "account") return { path: "/account", params: {} };
  if (parts[0] === "new") return { path: "/new", params: {} };
  if (parts[0] === "sessions" && parts[1]) return { path: "/sessions/:id", params: { id: parts[1] } };
  return { path: "/", params: {} };
}

/** Intercept in-app link clicks so the SPA does not reload. */
export function Link(props: { href: string; class?: string; children: preact.ComponentChildren; title?: string }) {
  const onClick = (e: MouseEvent) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) return;
    e.preventDefault();
    navigate(props.href);
  };
  return (
    <a href={props.href} class={props.class} title={props.title} onClick={onClick}>
      {props.children}
    </a>
  );
}
