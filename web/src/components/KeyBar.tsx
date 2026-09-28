import type { ComponentChildren } from "preact";
import { useRef, useState } from "preact/hooks";

// TapButton acts on the touch itself. On phones a tap on a <button> moves
// focus away from xterm's textarea, which closes the on-screen keyboard, and
// re-focusing afterwards opens it again: a flicker on every key. Preventing
// the default on mousedown is not enough on iOS, where the blur comes with
// the synthetic click after touchend. So a tap is handled at touchend with
// its default prevented (no click, no blur, no focus change); a touch that
// moved is the bar scrolling and is ignored; clicks still work for mice.
function TapButton(props: { onTap: () => void; class?: string; title?: string; pressed?: boolean; children: ComponentChildren }) {
  const start = useRef<{ x: number; y: number } | null>(null);
  return (
    <button
      class={props.class}
      title={props.title}
      aria-pressed={props.pressed}
      tabIndex={-1}
      onTouchStart={(e) => {
        const t = e.touches[0];
        start.current = t ? { x: t.clientX, y: t.clientY } : null;
      }}
      onTouchEnd={(e) => {
        const s = start.current;
        start.current = null;
        const t = e.changedTouches[0];
        if (!s || !t || Math.hypot(t.clientX - s.x, t.clientY - s.y) > 10) return;
        e.preventDefault();
        props.onTap();
      }}
      onTouchCancel={() => (start.current = null)}
      onMouseDown={(e) => e.preventDefault()}
      onClick={props.onTap}
    >
      {props.children}
    </button>
  );
}

// Sticky key bar for phones: keys the on-screen keyboard lacks. A Ctrl
// toggle applies to the next key sent (from the bar or typed). Keys are sent
// straight to the session and never touch focus: the keyboard stays as it
// is, and the ⌨ button is the one place that opens or closes it.
export function KeyBar(props: {
  onKey: (seq: string) => void;
  onKeyboard: () => void;
  keyboardOpen: boolean;
  onPaste: () => void;
  onSelect: () => void;
  onLinks: () => void;
}) {
  const [ctrl, setCtrl] = useState(false);

  const send = (seq: string) => {
    if (ctrl && seq.length === 1) {
      const code = seq.toUpperCase().charCodeAt(0);
      if (code >= 64 && code <= 95) seq = String.fromCharCode(code - 64);
      setCtrl(false);
    }
    props.onKey(seq);
  };

  const keys: [string, string][] = [
    ["Esc", "\x1b"],
    ["Tab", "\t"],
    ["↑", "\x1b[A"],
    ["↓", "\x1b[B"],
    ["←", "\x1b[D"],
    ["→", "\x1b[C"],
    ["^C", "\x03"],
    ["^D", "\x04"],
    ["^Z", "\x1a"],
    ["^L", "\x0c"],
    ["/", "/"],
    ["-", "-"],
    ["|", "|"],
    ["~", "~"],
    ["Enter", "\r"],
  ];

  return (
    <div class="keybar" onTouchStart={(e) => e.stopPropagation()}>
      <TapButton
        class={`action ${props.keyboardOpen ? "active" : ""}`}
        onTap={props.onKeyboard}
        title={props.keyboardOpen ? "Hide the keyboard" : "Show the keyboard"}
        pressed={props.keyboardOpen}
      >
        ⌨
      </TapButton>
      <TapButton class="action" onTap={props.onPaste} title="Paste from the clipboard">
        Paste
      </TapButton>
      <TapButton class="action" onTap={props.onSelect} title="Select and copy screen text">
        Select
      </TapButton>
      <TapButton class="action" onTap={props.onLinks} title="Links on screen">
        Links
      </TapButton>
      <TapButton class={ctrl ? "active" : ""} onTap={() => setCtrl(!ctrl)} pressed={ctrl}>
        Ctrl
      </TapButton>
      {keys.map(([label, seq]) => (
        <TapButton onTap={() => send(seq)}>{label}</TapButton>
      ))}
    </div>
  );
}
