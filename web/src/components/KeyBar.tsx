import { useState } from "preact/hooks";

// Sticky key bar for phones: keys the on-screen keyboard lacks. A Ctrl
// toggle applies to the next key sent (from the bar or typed).
export function KeyBar(props: {
  onKey: (seq: string) => void;
  onFocus: () => void;
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
    props.onFocus();
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
      <button
        class={`action ${props.keyboardOpen ? "active" : ""}`}
        onMouseDown={(e) => e.preventDefault()}
        onClick={props.onKeyboard}
        title={props.keyboardOpen ? "Hide the keyboard" : "Show the keyboard"}
        aria-pressed={props.keyboardOpen}
      >
        ⌨
      </button>
      <button class="action" onMouseDown={(e) => e.preventDefault()} onClick={props.onPaste} title="Paste from the clipboard">
        Paste
      </button>
      <button class="action" onMouseDown={(e) => e.preventDefault()} onClick={props.onSelect} title="Select and copy screen text">
        Select
      </button>
      <button class="action" onMouseDown={(e) => e.preventDefault()} onClick={props.onLinks} title="Links on screen">
        Links
      </button>
      <button class={ctrl ? "active" : ""} onClick={() => setCtrl(!ctrl)} aria-pressed={ctrl}>
        Ctrl
      </button>
      {keys.map(([label, seq]) => (
        <button onMouseDown={(e) => e.preventDefault()} onClick={() => send(seq)}>
          {label}
        </button>
      ))}
    </div>
  );
}
