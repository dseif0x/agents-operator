import { useEffect, useState } from "preact/hooks";
import { api } from "../api";
import { disablePush, enablePush, pushState, pushSupported, type PushState } from "../pwa";

/** iOS only delivers web push to an app added to the home screen. */
const iosBrowser = () => /iPhone|iPad|iPod/.test(navigator.userAgent) && !matchMedia("(display-mode: standalone)").matches;

function usePush() {
  const [state, setState] = useState<PushState | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    pushState()
      .then(setState)
      .catch(() => setState("unsupported"));
  }, []);
  const run = async (f: () => Promise<PushState>) => {
    setError("");
    try {
      setState(await f());
    } catch (e) {
      setError((e as Error).message);
    }
  };
  return { state, error, setError, enable: () => run(enablePush), disable: () => run(disablePush) };
}

/** NotificationsCard is the account page's switch for this device. */
export function NotificationsCard() {
  const { state, error, setError, enable, disable } = usePush();
  const [note, setNote] = useState("");
  const test = async () => {
    setError("");
    setNote("");
    try {
      const r = await api.pushTest();
      setNote(r.sent ? `Sent to ${r.sent} device(s).` : "No device is subscribed.");
    } catch (e) {
      setError((e as Error).message);
    }
  };
  return (
    <section class="card">
      <h3 style="margin:0">Notifications</h3>
      <p class="muted" style="margin:6px 0 10px;font-size:13px">
        Push notifications on this device when an agent writes in a chat, or a session waits for your permission or hits an error. Nothing comes
        for the page you are looking at.
      </p>
      {state === null && <div class="muted">Checking…</div>}
      {state === "unsupported" &&
        (iosBrowser() ? (
          <div class="muted">On iPhone and iPad: tap Share → “Add to Home Screen”, open agents-operator from there, then turn notifications on here.</div>
        ) : (
          <div class="muted">This browser does not support push notifications.</div>
        ))}
      {state === "denied" && <div class="muted">Notifications are blocked for this site; allow them in the browser's site settings, then reload.</div>}
      {state === "off" && (
        <button class="btn primary" onClick={enable}>
          Turn on for this device
        </button>
      )}
      {state === "on" && (
        <div class="row" style="gap:8px;flex-wrap:wrap">
          <span style="color:var(--ok);align-self:center">On for this device</span>
          <button class="btn small" onClick={test}>
            Send a test
          </button>
          <button class="btn small" onClick={disable}>
            Turn off
          </button>
        </div>
      )}
      {note && <div class="muted" style="margin-top:8px">{note}</div>}
      {error && <div class="error">{error}</div>}
    </section>
  );
}

const DISMISSED = "agents-operator.pushPromptDismissed";

/** PushPrompt offers notifications once, where they matter (the chat list). */
export function PushPrompt() {
  const { state, error, enable } = usePush();
  const [dismissed, setDismissed] = useState(() => {
    try {
      return localStorage.getItem(DISMISSED) === "1";
    } catch {
      return true;
    }
  });
  const ios = iosBrowser();
  if (dismissed || (state !== "off" && !(ios && !pushSupported()))) return null;
  const dismiss = () => {
    setDismissed(true);
    try {
      localStorage.setItem(DISMISSED, "1");
    } catch {
      /* fine */
    }
  };
  return (
    <div class="push-prompt">
      {ios && !pushSupported() ? (
        <span>For notifications, add agents-operator to your home screen (Share → Add to Home Screen) and open it from there.</span>
      ) : (
        <>
          <span>Get a notification when an agent writes to you.</span>
          <button class="btn small primary" onClick={enable}>
            Turn on
          </button>
        </>
      )}
      <button class="btn small" onClick={dismiss} title="Dismiss">
        ✕
      </button>
      {error && <div class="error">{error}</div>}
    </div>
  );
}
