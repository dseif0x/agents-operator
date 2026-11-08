// Previews for attachments: images and media inline, PDFs in the browser's
// viewer, text-like files in a sheet in the app (a phone has nothing to open
// a .txt or .log with), anything else a download.
import { useEffect, useState } from "preact/hooks";
import { createPortal } from "preact/compat";
import { renderMessage } from "./markdown";

export type PreviewKind = "image" | "video" | "audio" | "pdf" | "text" | "file";

const imageTypes = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
const videoTypes = new Set(["video/mp4", "video/webm", "video/quicktime"]);
const textTypes = new Set([
  "application/json",
  "application/xml",
  "application/yaml",
  "application/x-yaml",
  "application/toml",
  "application/javascript",
  "application/x-sh",
  "application/sql",
  "application/x-ndjson",
]);
const textExt =
  /\.(txt|text|md|markdown|log|json|jsonl|ndjson|ya?ml|toml|ini|conf|cfg|env|csv|tsv|xml|html?|css|scss|js|mjs|cjs|jsx|ts|tsx|go|mod|sum|py|rb|rs|java|kt|swift|c|h|cc|cpp|hpp|cs|php|sh|bash|zsh|fish|ps1|sql|diff|patch|dockerfile|makefile|gitignore|properties|tf|hcl|nix|lua|vue|svelte|graphql|proto)$/i;

/** previewKind is how an attachment is shown, by its type and name. */
export function previewKind(contentType: string, name: string): PreviewKind {
  const ct = contentType.split(";")[0].trim().toLowerCase();
  if (imageTypes.has(ct)) return "image";
  if (videoTypes.has(ct)) return "video";
  if (ct.startsWith("audio/")) return "audio";
  if (ct === "application/pdf") return "pdf";
  if (ct.startsWith("text/") || textTypes.has(ct) || textExt.test(name) || /^(Dockerfile|Makefile|README|LICENSE)$/i.test(name)) return "text";
  return "file";
}

/** The first bytes shown of a text file. */
const PREVIEW_BYTES = 1 << 20;

/** TextPreview shows a text file in a sheet: Markdown rendered, anything else as is. */
export function TextPreview({ name, url, size, onClose }: { name: string; url: string; size: number; onClose: () => void }) {
  const [text, setText] = useState<string | null>(null);
  const [err, setErr] = useState("");
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    let live = true;
    fetch(url, { headers: size > PREVIEW_BYTES ? { Range: `bytes=0-${PREVIEW_BYTES - 1}` } : {} })
      .then((r) => {
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        return r.text();
      })
      .then((t) => live && setText(t))
      .catch((e) => live && setErr((e as Error).message));
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    addEventListener("keydown", onKey);
    return () => {
      live = false;
      removeEventListener("keydown", onKey);
    };
  }, [url]);
  const md = /\.(md|markdown)$/i.test(name);
  const copy = async () => {
    if (text == null) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // clipboard denied: nothing to do
    }
  };
  return createPortal(
    <div class="file-preview" onClick={onClose}>
      <div class="file-preview-sheet" role="dialog" aria-label={name} onClick={(e) => e.stopPropagation()}>
        <div class="file-preview-head">
          <span class="name" title={name}>
            {name}
          </span>
          <button class="fp-btn" onClick={copy} disabled={text == null}>
            {copied ? "Copied" : "Copy"}
          </button>
          <a class="fp-btn" href={url} download={name}>
            Download
          </a>
          <button class="fp-btn" title="Close" onClick={onClose}>
            ✕
          </button>
        </div>
        <div class="file-preview-body">
          {err && <div class="file-preview-note">Cannot load the file: {err}</div>}
          {text == null && !err && <div class="file-preview-note">Loading…</div>}
          {text != null && (md ? <div class="md file-preview-md" dangerouslySetInnerHTML={{ __html: renderMessage(text, []) }} /> : <pre>{text}</pre>)}
          {text != null && size > PREVIEW_BYTES && <div class="file-preview-note">Showing the first 1 MB of {(size / (1 << 20)).toFixed(1)} MB. Download the file for the rest.</div>}
        </div>
      </div>
    </div>,
    document.body,
  );
}
