// The app as a PWA: the service worker (push notifications), presence
// reports (so the hub does not notify about the page you are looking at),
// and reopening the page you were last on.
import { api } from "./api";
import { navigate } from "./router";

const LAST_PATH = "agents-operator.lastPath";

/** Pages worth coming back to; login and the like are not. */
function remembered(path: string): boolean {
  return path === "/" || path.startsWith("/sessions/") || path.startsWith("/chats") || path === "/account" || path === "/new";
}

/** rememberPath stores the current page (called on every route change). */
export function rememberPath(path: string) {
  if (!remembered(path)) return;
  try {
    localStorage.setItem(LAST_PATH, path);
  } catch {
    /* private mode: nothing to remember */
  }
}

/** lastPath is the page to return to after logging in. */
export function lastPath(): string {
  try {
    const p = localStorage.getItem(LAST_PATH);
    return p && remembered(p) ? p : "/";
  } catch {
    return "/";
  }
}

/**
 * restorePath, called once at start: opening the app at "/" (a fresh tab,
 * the home-screen icon) goes back to the page that was open last.
 */
export function restorePath() {
  if (location.pathname !== "/" || location.search || history.length > 2) return;
  let last: string | null = null;
  try {
    last = localStorage.getItem(LAST_PATH);
  } catch {
    return;
  }
  if (last && last !== "/" && remembered(last)) navigate(last, true);
}

export const pushSupported = () => "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;

let registration: Promise<ServiceWorkerRegistration | null> | null = null;

/** registerServiceWorker registers /sw.js once and follows its "navigate" messages. */
export function registerServiceWorker(): Promise<ServiceWorkerRegistration | null> {
  if (!("serviceWorker" in navigator)) return Promise.resolve(null);
  if (!registration) {
    registration = navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch(() => null);
    navigator.serviceWorker.addEventListener("message", (e: MessageEvent) => {
      if (e.data?.type === "navigate" && typeof e.data.url === "string") {
        const u = new URL(e.data.url, location.origin);
        if (u.origin === location.origin) navigate(u.pathname);
      }
    });
  }
  return registration;
}

function urlBase64ToBytes(s: string): Uint8Array {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const raw = atob((s + pad).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0));
}

export type PushState = "unsupported" | "denied" | "off" | "on";

/** pushState is whether this browser gets notifications. */
export async function pushState(): Promise<PushState> {
  if (!pushSupported()) return "unsupported";
  if (Notification.permission === "denied") return "denied";
  const reg = await registerServiceWorker();
  const sub = await reg?.pushManager.getSubscription();
  return sub && Notification.permission === "granted" ? "on" : "off";
}

/** enablePush asks for permission (call it from a click) and subscribes. */
export async function enablePush(): Promise<PushState> {
  if (!pushSupported()) return "unsupported";
  const perm = await Notification.requestPermission();
  if (perm !== "granted") return perm === "denied" ? "denied" : "off";
  const reg = await registerServiceWorker();
  if (!reg) throw new Error("the service worker could not be registered");
  const { public_key } = await api.pushKey();
  let sub = await reg.pushManager.getSubscription();
  // A subscription made with another key (a reset hub) is useless.
  if (sub) {
    const key = sub.options.applicationServerKey;
    const same = key && btoa(String.fromCharCode(...new Uint8Array(key))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "") === public_key;
    if (!same) {
      await sub.unsubscribe();
      sub = null;
    }
  }
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: urlBase64ToBytes(public_key) as BufferSource });
  await api.pushSubscribe(sub.toJSON());
  return "on";
}

/** disablePush unsubscribes this browser. */
export async function disablePush(): Promise<PushState> {
  const reg = await registerServiceWorker();
  const sub = await reg?.pushManager.getSubscription();
  if (sub) {
    await api.pushUnsubscribe(sub.endpoint).catch(() => undefined);
    await sub.unsubscribe();
  }
  return "off";
}

let presenceTimer: ReturnType<typeof setInterval> | undefined;
let presencePath = "";

/**
 * reportPresence tells the hub which page is on screen, and repeats it while
 * visible, so it holds back notifications for that page. A hidden app
 * reports nothing (""), so they come through.
 */
export function reportPresence(path: string) {
  presencePath = path;
  const send = () => api.presence(document.visibilityState === "visible" ? presencePath : "").catch(() => undefined);
  send();
  if (!presenceTimer) {
    presenceTimer = setInterval(() => document.visibilityState === "visible" && send(), 30000);
    document.addEventListener("visibilitychange", send);
  }
}
