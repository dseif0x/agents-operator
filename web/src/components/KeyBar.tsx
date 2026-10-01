import type { ComponentChildren } from "preact";
import { useRef, useState } from "preact/hooks";

// TapButton acts on the touch itself. On phones a tap on a <button> moves
// focus away from xterm's textarea, which closes the on-screen keyboard;
// preventing the default on mousedown is not enough on iOS, where the blur
// comes with the synthetic click after touchend. So a tap is handled at
// touchend with its default prevented (no click, no blur, no focus change),
// a touch that moved is the bar scrolling and is ignored, and clicks still
// work for mice. The buttons carry no tabindex: Safari focuses anything
// with one on tap, and that blur is the one preventDefault cannot stop.
// With `repeat`, holding the button fires it again and again (arrows,
// backspace), like a real key.
function TapButton(props: { onTap: () => void; repeat?: boolean; class?: string; title?: string; pressed?: boolean; children: ComponentChildren }) {
  const start = useRef<{ x: number; y: number } | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const ticker = useRef<number | undefined>(undefined);
  const repeated = useRef(false);
  const stop = () => {
    clearTimeout(timer.current);
    clearInterval(ticker.current);
    timer.current = ticker.current = undefined;
  };
  return (
    <button
      class={props.class}
      title={props.title}
      aria-pressed={props.pressed}
      onTouchStart={(e) => {
        const t = e.touches[0];
        start.current = t ? { x: t.clientX, y: t.clientY } : null;
        repeated.current = false;
        if (props.repeat) {
          timer.current = window.setTimeout(() => {
            repeated.current = true;
            props.onTap();
            ticker.current = window.setInterval(props.onTap, 60);
          }, 350);
        }
      }}
      onTouchMove={(e) => {
        const s = start.current;
        const t = e.touches[0];
        if (s && t && Math.hypot(t.clientX - s.x, t.clientY - s.y) > 10) stop();
      }}
      onTouchEnd={(e) => {
        stop();
        const s = start.current;
        start.current = null;
        if (repeated.current) {
          e.preventDefault();
          return;
        }
        const t = e.changedTouches[0];
        if (!s || !t || Math.hypot(t.clientX - s.x, t.clientY - s.y) > 10) return;
        e.preventDefault();
        props.onTap();
      }}
      onTouchCancel={() => {
        stop();
        start.current = null;
      }}
      onMouseDown={(e) => e.preventDefault()}
      onClick={props.onTap}
    >
      {props.children}
    </button>
  );
}

// Sticky key bar for phones: keys the on-screen keyboard lacks, two rows
// deep. A Ctrl toggle applies to the next key sent (from the bar or typed).
// Keys go straight to the session and never touch focus; tapping the
// terminal is what opens the keyboard.
export function KeyBar(props: {
  onKey: (seq: string) => void;
  /** Whether the terminal had the keyboard up; read before the tap lands. */
  terminalFocused: () => boolean;
  /** Give focus back to the terminal, inside the tap, when a key took it. */
  refocus: () => void;
  onPaste: () => void;
  onSelect: () => void;
  onLinks: () => void;
}) {
  const [ctrl, setCtrl] = useState(false);
  // Whether the keyboard was up when the finger came down. Should a tap
  // still steal focus, giving it straight back inside the same gesture
  // keeps the keyboard where it was.
  const hadFocus = useRef(false);
  // The bar scrolls sideways by hand: preventing the default on touchstart
  // is what stops a tap from moving focus off the terminal on iOS (the
  // spec says no mouse events, hence no focus change, follow a cancelled
  // touchstart; a cancelled touchend only drops the click), and that also
  // switches off native scrolling.
  const bar = useRef<HTMLDivElement>(null);
  const drag = useRef<{ x: number; left: number } | null>(null);

  const send = (seq: string) => {
    if (ctrl && seq.length === 1) {
      const code = seq.toUpperCase().charCodeAt(0);
      if (code >= 64 && code <= 95) seq = String.fromCharCode(code - 64);
      setCtrl(false);
    }
    props.onKey(seq);
    if (hadFocus.current) props.refocus();
  };

  // [label, sequence, repeats while held]
  const keys: [string, string, boolean?][] = [
    ["Esc", "\x1b"],
    ["Tab", "\t", true],
    ["⇧Tab", "\x1b[Z"],
    ["↑", "\x1b[A", true],
    ["↓", "\x1b[B", true],
    ["←", "\x1b[D", true],
    ["→", "\x1b[C", true],
    ["⌫", "\x7f", true],
    ["Enter", "\r", true],
    ["^C", "\x03"],
    ["^D", "\x04"],
    ["^Z", "\x1a"],
    ["^L", "\x0c"],
    ["/", "/"],
    ["-", "-"],
    ["|", "|"],
    ["~", "~"],
  ];

  return (
    <div
      class="keybar"
      ref={bar}
      onTouchStart={(e) => {
        e.stopPropagation();
        e.preventDefault();
        hadFocus.current = props.terminalFocused();
        const t = e.touches[0];
        drag.current = t && bar.current ? { x: t.clientX, left: bar.current.scrollLeft } : null;
      }}
      onTouchMove={(e) => {
        const d = drag.current;
        const t = e.touches[0];
        if (d && t && bar.current) bar.current.scrollLeft = d.left - (t.clientX - d.x);
      }}
      onTouchEnd={() => (drag.current = null)}
      onTouchCancel={() => (drag.current = null)}
    >
      <TapButton class="action" onTap={props.onPaste} title="Paste from the clipboard">
        Paste
      </TapButton>
      <TapButton class="action" onTap={props.onSelect} title="Select and copy screen text">
        Select
      </TapButton>
      <TapButton class="action" onTap={props.onLinks} title="Links on screen">
        Links
      </TapButton>
      <TapButton class={ctrl ? "active" : ""} onTap={() => setCtrl(!ctrl)} pressed={ctrl} title="Apply Ctrl to the next key">
        Ctrl
      </TapButton>
      {keys.map(([label, seq, repeat]) => (
        <TapButton onTap={() => send(seq)} repeat={repeat}>
          {label}
        </TapButton>
      ))}
    </div>
  );
}
