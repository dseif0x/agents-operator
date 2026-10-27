// Chat messages are Markdown, written by the owner or by agents. They are
// rendered with marked and then sanitized with DOMPurify: no raw HTML
// survives that could run script, links are http(s), mailto or in-page,
// and images are left out (a chat's images are its attachments, served by
// the hub). Mentions (@handle) become links to #mention-<session id> that
// the chat view styles as chips and turns into navigation.
import { Marked } from "marked";
import DOMPurify from "dompurify";
import type { ChatMember } from "./api";

const md = new Marked({ gfm: true, breaks: true });

// Same rule as the hub (internal/session/chats.go mentionRE).
const mentionRE = /(^|[^A-Za-z0-9_@./])@([a-z]+_[a-z]+(?:_[0-9]+)?|everyone)\b/g;

const escapeMd = (s: string) => s.replace(/([\\`*_[\]#<>])/g, "\\$1");

let hooked = false;

function sanitize(html: string): string {
  if (!hooked) {
    hooked = true;
    DOMPurify.addHook("afterSanitizeAttributes", (node) => {
      if (node.tagName === "A") {
        const href = node.getAttribute("href") || "";
        if (/^https?:/i.test(href)) {
          node.setAttribute("target", "_blank");
          node.setAttribute("rel", "noopener noreferrer");
        }
      }
    });
  }
  return DOMPurify.sanitize(html, {
    ALLOWED_URI_REGEXP: /^(?:https?:|mailto:|#)/i,
    FORBID_TAGS: ["img", "style", "form", "input", "button", "textarea", "select", "iframe", "video", "audio", "svg", "math"],
    FORBID_ATTR: ["style", "class", "id"],
  });
}

/** renderMessage turns a message's Markdown into safe HTML, with mentions as links. */
export function renderMessage(text: string, members: ChatMember[]): string {
  const byHandle = new Map(members.map((m) => [m.handle, m]));
  const withMentions = text.replace(mentionRE, (all, pre: string, handle: string) => {
    if (handle === "everyone") return `${pre}[@everyone](#mention-everyone)`;
    const m = byHandle.get(handle);
    return m ? `${pre}[@${escapeMd(m.name)}](#mention-${m.session_id})` : all;
  });
  return sanitize(md.parse(withMentions, { async: false }) as string);
}

/** mentionedHandles lists the handles and @everyone written in text, in order. */
export function mentionedHandles(text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(mentionRE)) if (!out.includes(m[2])) out.push(m[2]);
  return out;
}
