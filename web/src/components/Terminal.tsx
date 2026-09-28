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
  focus: () => void;
}

interface Props {
  sessionId: string;
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
    focus: () => xterm.current?.focus(),
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
          else if (m.t === "error") term.write(`\r\n\x1b[31m[agenthub] ${m.message}\x1b[0m\r\n`);
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
    addEventListener("agenthub:theme", onTheme);
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
      removeEventListener("agenthub:theme", onTheme);
      document.removeEventListener("visibilitychange", onVisible);
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
      <div ref={el} style="height:100%" onTouchStart={() => xterm.current?.focus()} />
    </>
  );
});
