import type { ComponentChildren } from "preact";
import { useRef } from "preact/hooks";

// TapButton acts on the touch itself. On phones a tap on a <button> moves
// focus away from xterm's textarea, which closes the on-screen keyboard;
// preventing the default on mousedown is not enough on iOS, where the blur
// comes with the synthetic click after touchend. So a tap is handled at
// touchend with its default prevented (no click, no blur, no focus change),
// a touch that moved is the bar scrolling and is ignored, and clicks still
// work for mice. The buttons carry no tabindex: Safari focuses anything
// with one on tap, and that blur is the one preventDefault cannot stop.
// With `repeat`, holding the button fires it again and again (arrows,
// backspace), like a real key. Only sideways movement (the bar scrolling)
// cancels a tap: the phone keyboard sliding in or out moves the whole bar
// up or down under a resting finger, and that must not swallow the key.
const SCROLL_SLOP = 10;
function TapButton(props: { onTap: () => void; repeat?: boolean; class?: string; title?: string; pressed?: boolean; children: ComponentChildren }) {
  const start = useRef<{ x: number } | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const ticker = useRef<number | undefined>(undefined);
  const repeated = useRef(false);
  // The interval below outlives the render that started it: always call the
  // current handler.
  const onTap = useRef(props.onTap);
  onTap.current = props.onTap;
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
        start.current = t ? { x: t.clientX } : null;
        repeated.current = false;
        if (props.repeat) {
          timer.current = window.setTimeout(() => {
            repeated.current = true;
            onTap.current();
            ticker.current = window.setInterval(() => onTap.current(), 60);
          }, 350);
        }
      }}
      onTouchMove={(e) => {
        const s = start.current;
        const t = e.touches[0];
        if (s && t && Math.abs(t.clientX - s.x) > SCROLL_SLOP) stop();
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
        if (!s || !t || Math.abs(t.clientX - s.x) > SCROLL_SLOP) return;
        e.preventDefault();
        onTap.current();
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
// deep. Ctrl arms the terminal's modifier, which applies to the next key
// from the bar or typed on the keyboard (Ctrl, then b on the keyboard, is
// Ctrl+B). Keys go straight to the session and never touch focus: the
// keyboard stays up or down exactly as it was. Re-focusing the terminal
// after a key was what made arrow taps bring the keyboard up or drop it.
export function KeyBar(props: {
  /** Send one key; the terminal applies a pending Ctrl. */
  onKey: (seq: string) => void;
  ctrl: boolean;
  onCtrl: () => void;
  onPaste: () => void;
  onSelect: () => void;
  onLinks: () => void;
}) {
  // The bar scrolls sideways by hand: preventing the default on touchstart
  // is what stops a tap from moving focus off the terminal on iOS (the
  // spec says no mouse events, hence no focus change, follow a cancelled
  // touchstart; a cancelled touchend only drops the click), and that also
  // switches off native scrolling.
  const bar = useRef<HTMLDivElement>(null);
  const drag = useRef<{ x: number; left: number } | null>(null);

  const send = (seq: string) => props.onKey(seq);

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
      <TapButton class={props.ctrl ? "active" : ""} onTap={props.onCtrl} pressed={props.ctrl} title="Apply Ctrl to the next key, from the bar or the keyboard">
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
