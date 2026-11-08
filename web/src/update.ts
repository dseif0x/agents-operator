// Notices when the hub serves a newer UI than the one this page runs.
// An open page (an iOS home-screen app in particular) keeps its JavaScript
// across hub releases; the hub's index.html names the current bundle, so
// comparing it with ours tells whether a reload would bring a new version.

const current = (document.querySelector('script[type="module"][src*="/assets/"]') as HTMLScriptElement | null)?.getAttribute("src") ?? "";
const listeners = new Set<() => void>();
let outdated = false;
let last = 0;

/** checkForUpdate fetches the hub's index.html (at most every 30 s unless forced) and compares bundles. */
export async function checkForUpdate(force = false) {
  if (!current || outdated) return; // the dev server has no hashed bundle
  if (!force && Date.now() - last < 30_000) return;
  last = Date.now();
  try {
    const r = await fetch("/", { cache: "no-store", credentials: "same-origin" });
    if (!r.ok) return;
    const m = (await r.text()).match(/<script[^>]+type="module"[^>]+src="([^"]*\/assets\/[^"]+)"/);
    if (m && m[1] !== current) {
      outdated = true;
      listeners.forEach((fn) => fn());
    }
  } catch {
    // offline or the hub restarting: the next check tries again
  }
}

/** onUpdate calls fn once a newer UI is out; it returns a stop function. */
export function onUpdate(fn: () => void): () => void {
  listeners.add(fn);
  if (outdated) fn();
  return () => listeners.delete(fn);
}

// A page brought back to the front (resumed app, other tab) checks.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") checkForUpdate();
});
