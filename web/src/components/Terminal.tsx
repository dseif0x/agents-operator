import { forwardRef } from "preact/compat";
import { useEffect, useImperativeHandle, useRef, useState } from "preact/hooks";
import { Terminal as XTerm } from "@xterm/xterm";
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
  /** Open the on-screen keyboard when closed, close it when open. Returns the new state. */
  toggleKeyboard: () => boolean;
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
  /** Called when xterm gains or loses focus, i.e. the keyboard opens or closes on phones. */
  onFocusChange?: (focused: boolean) => void;
}

// Terminal wraps xterm.js and a reconnecting WebSocket to the hub. Binary
// frames are PTY bytes; text frames are JSON control messages.
export const Terminal = forwardRef<TerminalHandle, Props>(function Terminal({ sessionId, onFocusChange }, ref) {
  const focusChange = useRef(onFocusChange);
  focusChange.current = onFocusChange;
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
    toggleKeyboard: () => {
      const t = xterm.current;
      if (!t) return false;
      if (t.textarea && document.activeElement === t.textarea) {
        t.textarea.blur();
        return false;
      }
      t.focus();
      return true;
    },
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
      // Always swallow the synthetic mouse/click events iOS would send after
      // a touch. When a tap opens the keyboard the layout shrinks and the key
      // bar slides up under the finger, so the late click would land on a key
      // bar button and close the keyboard again (or send a stray key).
      // Links on phones go through the Links sheet instead.
      e.preventDefault();
      if (!t.moved) {
        if (e.timeStamp - t.t0 < 500 && document.activeElement !== term.textarea) term.focus();
        touch = null;
        return;
      }
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
    const onFocus = () => focusChange.current?.(true);
    const onBlur = () => focusChange.current?.(false);
    term.textarea?.addEventListener("focus", onFocus);
    term.textarea?.addEventListener("blur", onBlur);
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

    const connect = () => {
      if (closed) return;
      setStatus("connecting");
      setRetryIn(0);
      const s = new WebSocket(wsUrl(sessionId));
      s.binaryType = "arraybuffer";
      socket = s;
      ws.current = s;
      s.onopen = () => {
        backoff = 1000;
        setStatus("open");
        term.clear();
        fitAddon.fit();
        sendResize();
        term.focus();
      };
      s.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(ev.data));
          return;
        }
        try {
          const m = JSON.parse(ev.data as string) as { t: string; code?: number; cols?: number; rows?: number; message?: string };
          if (m.t === "exit") setExit(m.code ?? 0);
          else if (m.t === "hello") setExit(null);
          else if (m.t === "error") term.write(`\r\n\x1b[31m[agents-operator] ${m.message}\x1b[0m\r\n`);
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
    // Mobile keyboards change the visual viewport rather than the layout.
    visualViewport?.addEventListener("resize", refit);
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
      visualViewport?.removeEventListener("resize", refit);
      removeEventListener("agents-operator:theme", onTheme);
      document.removeEventListener("visibilitychange", onVisible);
      cancelAnimationFrame(inertia);
      term.textarea?.removeEventListener("focus", onFocus);
      term.textarea?.removeEventListener("blur", onBlur);
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
