import { forwardRef } from "preact/compat";
import { useEffect, useImperativeHandle, useRef, useState } from "preact/hooks";
import { Terminal as XTerm } from "@xterm/xterm";
import { ClipboardAddon } from "@xterm/addon-clipboard";
import { FitAddon } from "@xterm/addon-fit";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { Unicode11Addon } from "@xterm/addon-unicode11";
import { wsUrl } from "../api";
import { currentTheme, terminalTheme, type Theme } from "../theme";

export interface TerminalHandle {
  send: (data: string) => void;
  /** Send clipboard-style text as if typed (bracketed paste when the app asked for it). */
  paste: (text: string) => void;
  focus: () => void;
  /** Whether xterm's textarea is the active element (the keyboard is up, on phones). */
  hasFocus: () => boolean;
  /** The text of the screen plus scrollback, for the selectable overlay. */
  screenText: () => string;
  /** URLs currently visible in the buffer, newest last, de-duplicated. */
  links: () => string[];
  /** xterm's own selection when there is one. */
  selection: () => string;
}

const urlRE = /https?:\/\/[^\s"'<>`)\]]+/g;

/** Read the buffer as plain text; wrapped lines are joined back together. */
function bufferText(term: XTerm): string {
  const buf = term.buffer.active;
  const out: string[] = [];
  let acc = "";
  for (let i = 0; i < buf.length; i++) {
    const line = buf.getLine(i);
    if (!line) continue;
    const text = line.translateToString(true);
    if (line.isWrapped) {
      acc += text;
    } else {
      if (i > 0) out.push(acc);
      acc = text;
    }
  }
  out.push(acc);
  // Drop trailing blank lines but keep internal spacing.
  while (out.length && out[out.length - 1].trim() === "") out.pop();
  return out.join("\n");
}

interface Props {
  sessionId: string;
}

/** Phones and tablets: no hover, coarse pointer. */
const touchDevice = () => matchMedia("(hover: none) and (pointer: coarse)").matches;

/**
 * Raise the on-screen keyboard: every tap on the terminal does this, and
 * the phone's own gesture (swipe down, Done) closes it. A plain focus()
 * does nothing when the textarea is already focused (xterm focuses it on
 * its own, and a focus made outside a user gesture leaves the element
 * focused with the keyboard closed), so drop focus first; the focus that
 * follows is inside the gesture and brings the keyboard up.
 */
function raiseKeyboard(term: XTerm) {
  const ta = term.textarea;
  if (!ta) return;
  if (document.activeElement === ta) {
    // blur() then focus() in one task is coalesced into nothing by iOS, so
    // move focus to a throwaway input first: two real focus changes, and
    // the keyboard follows the last one.
    const decoy = document.createElement("input");
    decoy.setAttribute("aria-hidden", "true");
    decoy.style.cssText = "position:fixed;top:0;left:0;width:1px;height:1px;opacity:0;border:0;padding:0;font-size:16px";
    document.body.appendChild(decoy);
    decoy.focus();
    ta.focus();
    decoy.remove();
    return;
  }
  term.focus();
}

/**
 * Replay recorded at another size: PTY output is full of cursor moves
 * and line erases that only make sense at the width it was drawn for, so
 * writing it into a terminal of a different width (a phone attaching to a
 * session a desktop was using, or to one nobody had sized yet) shows
 * shredded text. Run it through a headless terminal of the original size
 * and hand back what ended up on its screen and scrollback as plain
 * styled text, which wraps cleanly anywhere.
 */
async function reflowReplay(bytes: Uint8Array, cols: number, rows: number): Promise<string> {
  const [{ Terminal: Headless }, { SerializeAddon }] = await Promise.all([import("@xterm/headless"), import("@xterm/addon-serialize")]);
  const h = new Headless({ cols, rows, scrollback: 5000, allowProposedApi: true });
  const ser = new SerializeAddon();
  h.loadAddon(ser);
  try {
    await new Promise<void>((resolve) => h.write(bytes, resolve));
    return ser.serialize({ scrollback: 5000 });
  } finally {
    h.dispose();
  }
}

// Terminal wraps xterm.js and a reconnecting WebSocket to the hub. Binary
// frames are PTY bytes; text frames are JSON control messages.
export const Terminal = forwardRef<TerminalHandle, Props>(function Terminal({ sessionId }, ref) {
  const el = useRef<HTMLDivElement>(null);
  const ws = useRef<WebSocket | null>(null);
  const xterm = useRef<XTerm | null>(null);
  const fit = useRef<FitAddon | null>(null);
  const [status, setStatus] = useState<"connecting" | "open" | "closed">("connecting");
  const [exit, setExit] = useState<number | null>(null);
  const [retryIn, setRetryIn] = useState(0);

  useImperativeHandle(ref, () => ({
    send: (data: string) => {
      if (ws.current?.readyState === WebSocket.OPEN) ws.current.send(new TextEncoder().encode(data));
    },
    paste: (text: string) => xterm.current?.paste(text),
    focus: () => xterm.current?.focus(),
    hasFocus: () => !!xterm.current?.textarea && document.activeElement === xterm.current.textarea,
    screenText: () => (xterm.current ? bufferText(xterm.current) : ""),
    links: () => {
      const text = xterm.current ? bufferText(xterm.current) : "";
      const seen = new Set<string>();
      for (const m of text.matchAll(urlRE)) seen.add(m[0]);
      return [...seen];
    },
    selection: () => xterm.current?.getSelection() ?? "",
  }));

  useEffect(() => {
    if (!el.current) return;
    const term = new XTerm({
      allowProposedApi: true,
      cursorBlink: true,
      fontSize: matchMedia("(max-width: 640px)").matches ? 12 : 14,
      fontFamily: 'ui-monospace, "JetBrains Mono", "Fira Code", Menlo, Consolas, monospace',
      scrollback: 5000,
      theme: terminalTheme(currentTheme()),
      macOptionIsMeta: true,
      allowTransparency: false,
    });
    const fitAddon = new FitAddon();
    term.loadAddon(fitAddon);
    term.loadAddon(new WebLinksAddon());
    // OSC 52: TUIs like Claude Code copy their own selections by asking the
    // terminal to set the clipboard ("sent N chars via OSC 52"). Without
    // this addon xterm drops that request on the floor.
    term.loadAddon(new ClipboardAddon());
    term.loadAddon(new Unicode11Addon());
    term.unicode.activeVersion = "11";
    term.open(el.current);

    // Touch handling. xterm leaves touch to the browser, and on iOS a drag
    // with the keyboard open pans the visual viewport instead of scrolling
    // the buffer, while any touch used to focus the terminal and pop the
    // keyboard. So: consume drags here and scroll the buffer ourselves
    // (with a little inertia), and treat only a short tap as "focus".
    const host = el.current;
    let touch: { x: number; y: number; lastY: number; lastT: number; t0: number; moved: boolean; acc: number; v: number } | null = null;
    let inertia = 0;
    const rowHeight = () => Math.max(8, host.clientHeight / Math.max(1, term.rows));
    const scrollBy = (px: number) => {
      if (!touch) return;
      touch.acc += px / rowHeight();
      const lines = Math.trunc(touch.acc);
      if (lines !== 0) {
        touch.acc -= lines;
        term.scrollLines(lines);
      }
    };
    // Pinch-zoomed in, relative to how the page loaded (Safari's per-site
    // page zoom shows up in `scale` too and must not count): a drag then
    // pans the zoomed page and is left to the browser. Taps still focus.
    const baseScale = visualViewport?.scale ?? 1;
    const zoomed = () => (visualViewport?.scale ?? 1) > baseScale * 1.1;
    const onTouchStart = (e: TouchEvent) => {
      if (e.touches.length !== 1) return;
      cancelAnimationFrame(inertia);
      const p = e.touches[0];
      touch = { x: p.clientX, y: p.clientY, lastY: p.clientY, lastT: e.timeStamp, t0: e.timeStamp, moved: false, acc: 0, v: 0 };
    };
    const onTouchMove = (e: TouchEvent) => {
      if (!touch || e.touches.length !== 1) return;
      const p = e.touches[0];
      if (!touch.moved && Math.hypot(p.clientX - touch.x, p.clientY - touch.y) > 8) touch.moved = true;
      if (!touch.moved) return;
      if (zoomed()) {
        touch = null; // a pan, not a scroll and not a tap
        return;
      }
      e.preventDefault(); // keep the browser from panning the viewport
      const dy = touch.lastY - p.clientY;
      const dt = Math.max(1, e.timeStamp - touch.lastT);
      touch.v = dy / dt;
      touch.lastY = p.clientY;
      touch.lastT = e.timeStamp;
      scrollBy(dy);
    };
    const onTouchEnd = (e: TouchEvent) => {
      if (!touch) return;
      const t = touch;
      if (!t.moved) {
        // A tap: let it become a click, and raise the keyboard from the
        // click handler below. A click is the one activation iOS honours
        // for focus() in every state this page has been found in; focus()
        // from a cancelled touchend was not. touch-action:none on the host
        // means the click comes at once, before the keyboard moves the
        // layout, so it cannot land on the key bar.
        touch = null;
        return;
      }
      // A drag: swallow the synthetic mouse/click events that would follow.
      e.preventDefault();
      // Inertia: keep scrolling with the last velocity, decaying.
      let v = t.v;
      let last = performance.now();
      const step = (now: number) => {
        const dt = now - last;
        last = now;
        v *= Math.pow(0.94, dt / 16);
        if (Math.abs(v) < 0.02) {
          touch = null;
          return;
        }
        scrollBy(v * dt);
        inertia = requestAnimationFrame(step);
      };
      inertia = requestAnimationFrame(step);
    };
    host.addEventListener("touchstart", onTouchStart, { passive: true });
    host.addEventListener("touchmove", onTouchMove, { passive: false });
    host.addEventListener("touchend", onTouchEnd, { passive: false });
    host.addEventListener("touchcancel", () => (touch = null), { passive: true });
    // Tap to raise the keyboard (phones). Skipped when xterm's own mousedown
    // handling focused the textarea a moment ago, so the keyboard is not
    // bounced by the focus hop in raiseKeyboard.
    let focusedAt = 0;
    const onTextareaFocus = () => (focusedAt = Date.now());
    term.textarea?.addEventListener("focus", onTextareaFocus);
    const onClick = () => {
      if (!touchDevice()) return;
      if (document.activeElement === term.textarea && Date.now() - focusedAt < 700) return;
      raiseKeyboard(term);
    };
    host.addEventListener("click", onClick);

    // WebGL renderer with a canvas/DOM fallback when the context is lost or unavailable.
    import("@xterm/addon-webgl")
      .then(({ WebglAddon }) => {
        try {
          const webgl = new WebglAddon();
          webgl.onContextLoss(() => webgl.dispose());
          term.loadAddon(webgl);
        } catch {
          /* DOM renderer stays */
        }
      })
      .catch(() => undefined);
    fitAddon.fit();
    xterm.current = term;
    fit.current = fitAddon;

    const encoder = new TextEncoder();
    let socket: WebSocket | null = null;
    let closed = false;
    let backoff = 1000;
    let retryTimer: number | undefined;
    let countdown: number | undefined;

    const sendResize = () => {
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ t: "resize", cols: term.cols, rows: term.rows }));
      }
    };

    // Writes go through one promise chain so a replay that is being
    // reflowed (async) still lands before the live output that follows it.
    let writes: Promise<unknown> = Promise.resolve();
    const enqueue = (job: () => Promise<string | Uint8Array | null>) => {
      writes = writes
        .then(job)
        .then((data) => {
          if (data !== null && !closed) term.write(data);
        })
        .catch(() => undefined);
    };

    // Hold-to-repeat for the native keyboard's Backspace. Phone keyboards
    // repeat a held Backspace only while each press actually deletes
    // something from the field they edit, and xterm keeps its hidden
    // textarea empty (and cancels the key before it reaches the field), so
    // a held key sent one DEL and stopped. On touch devices the textarea
    // carries a run of spaces, Backspace is left to the browser (xterm is
    // told to ignore it), and each real deletion is turned into a DEL for
    // the session, after which the spaces are put back at once so the
    // next repeat has something to delete again. xterm's own diffing of
    // the textarea runs on a timer and sees no change by then. Word and
    // line deletions map to the readline keys. Nothing is touched during
    // IME composition.
    const sentinel = " ".repeat(64);
    const ta = term.textarea;
    let composing = false;
    const prime = () => {
      if (!ta || composing || ta.value.startsWith(sentinel)) return;
      ta.value = sentinel + ta.value.replace(/^ +/, "");
      try {
        ta.setSelectionRange(ta.value.length, ta.value.length);
      } catch {
        /* not focused */
      }
    };
    const primeLater = () => setTimeout(prime, 0);
    const deletions: Record<string, string> = {
      deleteContentBackward: "\x7f",
      deleteWordBackward: "\x17",
      deleteSoftLineBackward: "\x15",
      deleteHardLineBackward: "\x15",
    };
    const onInput = (e: Event) => {
      if (composing) return;
      const seq = deletions[(e as InputEvent).inputType];
      if (seq && socket?.readyState === WebSocket.OPEN) {
        socket.send(encoder.encode(seq));
        prime(); // the field shrank: refill it before the next repeat
      } else {
        primeLater();
      }
    };
    const onCompositionStart = () => (composing = true);
    const onCompositionEnd = () => {
      composing = false;
      primeLater();
    };
    if (ta && touchDevice()) {
      term.attachCustomKeyEventHandler((e) => {
        // Let Backspace reach the field so the browser deletes, repeats
        // and reports it through the input event above. With nothing to
        // delete (the sentinel is only there once focused) send the DEL
        // here so the key is never lost.
        if (e.key === "Backspace" && !e.ctrlKey && !e.altKey && !e.metaKey) {
          if (e.type === "keydown" && ta.value.length === 0 && socket?.readyState === WebSocket.OPEN) socket.send(encoder.encode("\x7f"));
          return false;
        }
        return true;
      });
      ta.addEventListener("input", onInput);
      ta.addEventListener("focus", prime);
      ta.addEventListener("keyup", primeLater);
      ta.addEventListener("compositionstart", onCompositionStart);
      ta.addEventListener("compositionend", onCompositionEnd);
      prime();
    }

    const connect = () => {
      if (closed) return;
      setStatus("connecting");
      setRetryIn(0);
      const s = new WebSocket(wsUrl(sessionId));
      s.binaryType = "arraybuffer";
      socket = s;
      ws.current = s;
      // Size the replay was drawn at, when it differs from ours.
      let replayAt: { cols: number; rows: number } | null = null;
      s.onopen = () => {
        backoff = 1000;
        setStatus("open");
        term.clear();
        fitAddon.fit();
        sendResize();
        // On a phone a focus outside a tap leaves the textarea focused with
        // the keyboard closed, which then needs two taps to open it.
        if (!touchDevice()) term.focus();
      };
      s.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          const bytes = new Uint8Array(ev.data);
          if (replayAt) {
            const at = replayAt;
            replayAt = null;
            enqueue(() => reflowReplay(bytes, at.cols, at.rows).catch(() => bytes));
          } else {
            enqueue(async () => bytes);
          }
          return;
        }
        try {
          const m = JSON.parse(ev.data as string) as { t: string; code?: number; cols?: number; rows?: number; scrollback?: boolean; message?: string };
          if (m.t === "exit") setExit(m.code ?? 0);
          else if (m.t === "hello") {
            setExit(null);
            replayAt = m.scrollback && m.cols && m.rows && (m.cols !== term.cols || m.rows !== term.rows) ? { cols: m.cols, rows: m.rows } : null;
          } else if (m.t === "error") enqueue(async () => `\r\n\x1b[31m[agents-operator] ${m.message}\x1b[0m\r\n`);
        } catch {
          /* ignore */
        }
      };
      s.onclose = () => {
        if (socket !== s) return;
        socket = null;
        ws.current = null;
        if (closed) return;
        setStatus("closed");
        const wait = backoff;
        backoff = Math.min(backoff * 2, 30000);
        let left = Math.round(wait / 1000);
        setRetryIn(left);
        clearInterval(countdown);
        countdown = window.setInterval(() => {
          left -= 1;
          setRetryIn(Math.max(left, 0));
        }, 1000);
        retryTimer = window.setTimeout(() => {
          clearInterval(countdown);
          connect();
        }, wait);
      };
      s.onerror = () => s.close();
    };

    const onData = term.onData((d) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(encoder.encode(d));
    });
    const onBinary = term.onBinary((d) => {
      if (socket?.readyState === WebSocket.OPEN) {
        const bytes = new Uint8Array(d.length);
        for (let i = 0; i < d.length; i++) bytes[i] = d.charCodeAt(i) & 255;
        socket.send(bytes);
      }
    });
    const onResize = term.onResize(sendResize);

    const refit = () => fitAddon.fit();
    const ro = new ResizeObserver(refit);
    ro.observe(el.current);
    const onTheme = (e: Event) => {
      term.options.theme = terminalTheme((e as CustomEvent<Theme>).detail);
    };
    addEventListener("agents-operator:theme", onTheme);
    // Reconnect promptly when the tab comes back.
    const onVisible = () => {
      if (document.visibilityState === "visible" && !socket && !closed) {
        clearTimeout(retryTimer);
        clearInterval(countdown);
        connect();
      }
    };
    document.addEventListener("visibilitychange", onVisible);

    connect();

    return () => {
      closed = true;
      clearTimeout(retryTimer);
      clearInterval(countdown);
      ro.disconnect();
      removeEventListener("agents-operator:theme", onTheme);
      document.removeEventListener("visibilitychange", onVisible);
      cancelAnimationFrame(inertia);
      if (ta) {
        ta.removeEventListener("input", onInput);
        ta.removeEventListener("focus", prime);
        ta.removeEventListener("keyup", primeLater);
        ta.removeEventListener("compositionstart", onCompositionStart);
        ta.removeEventListener("compositionend", onCompositionEnd);
      }
      host.removeEventListener("click", onClick);
      term.textarea?.removeEventListener("focus", onTextareaFocus);
      host.removeEventListener("touchstart", onTouchStart);
      host.removeEventListener("touchmove", onTouchMove);
      host.removeEventListener("touchend", onTouchEnd);
      onData.dispose();
      onBinary.dispose();
      onResize.dispose();
      socket?.close();
      term.dispose();
      xterm.current = null;
      ws.current = null;
    };
  }, [sessionId]);

  return (
    <>
      {status !== "open" && (
        <div class="banner">
          {status === "connecting" ? "Connecting…" : `Disconnected. Reconnecting${retryIn ? ` in ${retryIn}s` : "…"}`}
        </div>
      )}
      {exit !== null && status === "open" && (
        <div class="banner" style="top:auto;bottom:0">
          Agent exited with code {exit}. Use “Restart agent” to relaunch it; the pod and volume are untouched.
        </div>
      )}
      <div ref={el} class="term-host" style="height:100%" />
    </>
  );
});
