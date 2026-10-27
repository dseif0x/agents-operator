# Agent chats (plan)

Status: proposal, nothing implemented. It builds on the accounts work (#64) and single attach (#62). It replaces the earlier "crews" draft: instead of fixed groups with a coordinator, the owner creates **chats**, like groups in Discord or WhatsApp, and adds agent sessions to them.

## What it is for

Today every session is an island. To get an implementer, a reviewer and a tester working on one change, the user copies text between three terminals. With chats:

- **Chats:** the owner creates any number of chats and adds sessions to them. A session can be in several chats. Each chat looks and works like a group chat: a message list, an input box, images, and `@mentions`.
- **Personas:** each session gets a **persona**, free-text notes on personality and role ("You are the reviewer. Be blunt, ask for tests."). It is applied as a system prompt for every agent type.
- **Mentions:** `@reviewer` addresses one agent and `@everyone` addresses every agent in the chat. A mentioned agent receives the message, works on it in its own session, and answers in the chat.
- **Agents talk to agents** the same way. They can mention each other, so one agent can hand work to another, with a loop guard.

**Non-goals:**
- **No shared filesystem.** Work moves through git branches; chats carry the conversation, images and short notes.
- **No chats across users.**
- **No agent framework in the pod.** Claude Code, Codex and OpenCode stay unmodified and use chats through an MCP server.

## Constraints from what exists

- **Pods cannot reach the hub.** The NetworkPolicy lets only the hub reach a runner, on 7681. A prompt-injected agent therefore cannot call the hub API, and chats must keep that property.
- **The hub already talks to every runner:** `/status` polling every 10 s, one-shot control WebSockets (`export_login`, `restart`), and the agent's own activity through hooks (working, waiting for input, needs permission, error, and the last assistant message at the end of a turn).
- **All three CLIs speak MCP:** Claude Code (settings or `--mcp-config`), Codex (`mcp_servers` in config.toml) and OpenCode (the `mcp` key in the config the runner already generates). One server, run by `agent-runner`, covers all three.
- **The UI already has an SSE stream** for session changes (the broker in `internal/session`), which chat events can use too.

## Design

### 1. Personas

- **The setting:** a session setting `persona`, at most 8 KB of Markdown, edited on the session form next to the name. It replaces today's OpenCode-only "system prompt" field and covers every agent:
  - **Claude Code:** started with `--append-system-prompt <persona>`.
  - **Codex:** a section of the `~/.codex/AGENTS.md` the runner already generates.
  - **OpenCode:** the existing instructions file, or the build agent's prompt in "replace" mode.
  - **Shell:** ignored.
- **Chat identity:** each session gets a **handle** (the name used in `@mentions`, defaulting to the session name made mention-safe: `fix-login-bug` → `@fix-login-bug`), plus an optional emoji or colour for its avatar in the chat.
- **Context:** the runner also tells the agent which chats it is in and how to use the chat tools (a generated AGENTS.md section), so the persona can stay about character and role.
- **Restarts:** changing the persona or handle takes effect on the next start, like every other setting.

### 2. Chats

- **Membership:** a chat has a name, members (sessions of the owner) and messages. The owner is always in it. Sessions can be added and removed at any time; a stopped member is shown greyed out and gets its mentions when it next starts (see delivery).
- **Messages** have:
  - **author:** the owner or a session;
  - **text:** Markdown, at most 32 KB;
  - **attachments:** images, at most 10 MB each and 5 per message;
  - **mentions:** session ids, or everyone, resolved by the hub from `@handle` and `@everyone`.

### 3. Uplink: runner → hub requests over a hub-opened WebSocket

For each running session that is in at least one chat, the hub keeps one long-lived WebSocket to the runner: `/ws?uplink=1`, authenticated with the runner token like the other control connections. Runner→hub requests and hub→runner events both flow over it. The hub opens it, so the network direction stays hub → pod.

- **Framing:** requests are `{"id": n, "op": "...", "args": {...}}`, answered with `{"id": n, "ok": ..., "result" | "error": ...}`. Events look like `{"event": "message", "chat": ..., "message": ...}`.
- **Reconnects:** the hub reopens a dropped connection on the next poll (within 10 s). Calls made meanwhile wait up to 30 s, then fail with an error the tool shows the agent.
- **Leadership:** only the leader hub holds uplinks.
- **Attachments** travel over the same socket in chunks: images an agent sends up, and images it receives down.

### 4. `agent-runner mcp`: the tools the agent sees

A stdio MCP server, registered by the runner with the session's CLI:

| Tool | What it does |
|---|---|
| `chats_list` | the chats this session is in, with members (handle, persona summary, state) and unread counts |
| `chat_read(chat, since?, limit?)` | messages, oldest first; images come as file paths (see below) |
| `chat_send(chat, text, images?)` | post Markdown, optionally with images given as paths in the workspace; `@handle` and `@everyone` in the text mention |
| `chat_members(chat)` | who is in the chat and what they are doing right now (activity) |

That's all for phase 1. The task board and session-control tools from the earlier draft can come back later as extra tools if chats alone are not enough (see phases).

**Images:**
- **Received images** are written by the runner to `~/chats/<chat>/<message>-<n>.<ext>`, and the message text carries the path. Claude Code, Codex and OpenCode all read images from paths.
- **Sending:** an agent sends an image by passing a path, such as a screenshot it took or a chart it rendered. The runner accepts only files inside `/workspace` and the agent's temp directory, at most 10 MB, PNG, JPEG, GIF or WebP.

### 5. Delivery: how a mentioned agent gets the message

Agents are woken by mentions, not by every message. Otherwise a busy group would keep every agent typing.

- **Idle agent** (activity "waiting for input"): the runner types the message into the terminal as a prompt, for example:
  ```
  [chat "launch" · from @dan] @reviewer can you look at the diff on feat/x? (reply with chat_send to "launch")
  ```
  This is how a human would hand it over, so every CLI handles it the same way. The owner sees it in the terminal too.
- **Busy agent:** the message is queued. For Claude Code, the `Stop` hook the runner already installs blocks the end of the turn with "1 new mention in "launch"; read it with chat_read" so the agent picks it up straight away. Codex and OpenCode get it once they go idle.
- **Needs permission or error:** nothing is typed. The mention waits, and the chat shows it as "queued".
- **Stopped session:** mentions wait in the hub and are delivered after the next start.
- **Answering:** the agent normally answers with `chat_send`. If a turn that started from a mention ends without one, the runner posts the turn's last assistant message (which the `Stop` hook already reports) as the reply, marked "auto-reply", so a chat never silently swallows a request. This can be turned off per chat.
- **Unmentioned messages** are not delivered, but the agent can read them with `chat_read`. The typed prompt says how many unread messages came before the mention, so the agent knows when context is worth fetching.

**Loop guard:** agents mentioning agents could ping-pong forever. The hub counts agent-authored messages since the owner last wrote in the chat. After 20 (configurable per chat), mentions from agents are stored but not delivered, and the chat shows "paused: agents waiting for you" until the owner writes something. The owner can also mute a chat or a single member.

### 6. Web UI

- **Layout,** like Discord or WhatsApp:
  - **Chat list** (left, or the first screen on a phone): chats with the last message, an unread badge, and a "needs you" marker when a member waits for permission.
  - **Chat view:** messages grouped by author with avatar, handle and time.
    - **Markdown** is rendered with code blocks, tables, lists and links, using `marked` plus `DOMPurify`, with syntax highlighting for code.
    - **Images** appear inline and open full size on tap.
    - **Activity:** "@reviewer is working…" appears while a mentioned agent is busy, and "queued" shows on mentions not yet delivered.
  - **Input:**
    - Markdown text with `@` autocomplete over the chat's handles and `@everyone`.
    - Images by paste, drag and drop, or the file picker (phone camera included).
    - Enter sends; Shift+Enter starts a new line.
  - **Member panel:** add and remove sessions, each member's persona (with a link to edit it), current activity, and a link to its terminal.
- **Navigation:** clicking an agent's handle opens its terminal. The session page gets a "Chats" drawer tab listing the session's chats.
- **Live updates** come over the existing SSE stream (new messages, read state, member activity).
- **Images** are served by the hub from `/api/v1/chats/{id}/attachments/{aid}`, behind the usual login cookie.

### 7. Data model

```sql
sessions           + persona text, handle text, avatar text           -- handle unique per owner
chats              (id, owner_id, name, agent_message_limit, auto_reply, muted, created_at)
chat_members       (chat_id, session_id, muted, added_at)
chat_messages      (id, chat_id, author_session /* null = owner */, text, mentions jsonb /* session ids or ["*"] */, auto_reply bool, created_at)
chat_attachments   (id, message_id, content_type, size, data bytea)   -- Postgres, see below
chat_deliveries    (message_id, session_id, state /* queued|delivered|skipped */, delivered_at)
chat_reads         (chat_id, reader /* owner or session */, last_message_id)
```

**Attachments in Postgres:** a homelab chat produces modest volumes, and Postgres is already backed up (CNPG and its S3 backups). That avoids a second store and keeps the hub's rolling updates free of RWO volumes. A per-user quota (default 2 GB) caps it. Object storage (the R2 credentials already exist in the cluster) can replace it later behind the same API.

Messages are kept until the chat is deleted. Deleting a session removes it from its chats but keeps its messages, shown as "@name (deleted)".

### 8. API

- `GET/POST /chats`, `GET/PUT/DELETE /chats/{id}`: list, create, rename, settings, delete.
- `POST/DELETE /chats/{id}/members/{session}`.
- `GET /chats/{id}/messages?before=&limit=` (paged) and `POST /chats/{id}/messages` (multipart: text plus images).
- `GET /chats/{id}/attachments/{aid}`.
- `POST /chats/{id}/read`.
- The persona, handle and avatar are new fields on session create and edit.

### 9. Security

- **Scope:** a session only ever sees the chats it is a member of. The uplink checks membership on every call, using the session the connection belongs to.
- **Rendering:** Markdown from agents is sanitized before it is rendered (DOMPurify; no raw HTML, `javascript:` or `data:` links, and images only from the hub's own attachment URLs), so an agent cannot inject script into the owner's browser.
- **Image paths:** limited to the workspace and the temp directory, so an agent cannot post `~/.ssh` contents as "images". The content type is checked by sniffing, not by extension.
- **Events:** chat messages are events on the session too, so the session's event log shows what it was asked and what it answered.

## Phases

1. **Personas and handles:** the session form field, applied to Claude Code, Codex and OpenCode. Useful on its own.
2. **Chats with text:** data model, API, chat UI with Markdown and mentions, uplink, `agent-runner mcp` (`chats_list`, `chat_read`, `chat_send`, `chat_members`), idle delivery, auto-reply, loop guard.
3. **Images:** upload and inline display in the UI, attachments over the uplink, image paths both ways.
4. **Polish:** Claude Code `Stop`-hook delivery while busy, unread badges and browser notifications, chat templates ("implementer + reviewer + tester" with personas), muting.
5. **Later, if wanted:** a shared task list per chat, or a tool for an agent to start helper sessions into a chat (with the limits from the earlier draft: never wider settings than its own).

Each phase ships on its own. Sessions that are in no chat behave exactly as today.

## Testing

- **Runner:**
  - the MCP server against a fake uplink;
  - delivery: idle → typed prompt, busy → queued, permission → nothing typed;
  - auto-reply when a turn ends without `chat_send`;
  - image path checks: outside the workspace refused, wrong type refused.
- **Hub:**
  - mention parsing (`@handle`, `@everyone`, unknown handles left as text);
  - the delivery queue for stopped sessions;
  - the loop guard;
  - membership checks on the uplink;
  - attachment quota;
  - Markdown sanitizing (a test corpus of XSS payloads rendered through the UI's pipeline).
- **UI:** Playwright against the built UI with a mocked API: sending, `@` autocomplete, Markdown and image rendering, phone layout.
- **End to end in `testing`:** two Claude Code sessions with personas in one chat. The owner mentions one, it mentions the other, both answer in the chat, and an image round-trips.

## Open questions

1. **Handles:** session name by default, editable. Or should handles be fixed once set, so old messages keep pointing at the right agent after a rename?
2. **Unmentioned messages:** "mentions only" as the delivery default. Do you want a per-chat switch for "agents see every message" (a small group where everyone listens)?
3. **Auto-reply:** on by default, or should agents have to use `chat_send` explicitly?
4. **Loop guard:** is 20 agent messages without you a sensible default?
5. **Persona vs. system prompt:** fold the existing OpenCode system prompt and its "replace" mode into the persona, or keep "replace" as an advanced OpenCode-only option?
