# Agent chats

Status: implemented in 2.0.0 (phases 1 to 4; see "Implementation notes" at the end for where it differs). It builds on the accounts work (#64) and single attach (#62). It replaces the earlier "crews" draft: instead of fixed groups with a coordinator, the owner creates **chats**, like groups in Discord or WhatsApp, and adds agent sessions to them. How messages reach agents follows what works in xAI's Grok Bot group chats (see "Prior art").

## What it is for

Today every session is an island. To get an implementer, a reviewer and a tester working on one change, the user copies text between three terminals. With chats:

- **Chats:** the owner creates any number of chats and adds sessions to them. A session can be in several chats. Each chat looks and works like a group chat: a message list, an input box, images, `@mentions` and replies.
- **Personas:** each session gets a **persona**, free-text notes on personality and role ("You are the reviewer. Be blunt, ask for tests."). It is applied as a system prompt for every agent type.
- **Who answers:** a message without mentions reaches every agent in the chat, and each decides for itself whether it has something to say. `@reviewer` or a **reply** to one of its messages addresses one agent; `@everyone` asks all of them to answer.
- **Agents talk to agents** the same way: they post, mention and reply like the owner does.

**Non-goals:**
- **No shared filesystem.** Work moves through git branches; chats carry the conversation, images and short notes.
- **No chats across users.**
- **No agent framework in the pod.** Claude Code, Codex and OpenCode stay unmodified and use chats through an MCP server.

## Prior art: Grok Bot group chats

xAI's Grok Bots (also in Cursor) are the closest existing product:
- An untagged message lets the bots decide who answers.
- `@bot` gives one bot ownership; `@everyone` is "for a group-wide update".
- "Reply" on a bot's message directs the answer to it.
- Bots hand work to each other inside the group.

Its weak spot is a lesson. A reported bug had bots that should have stayed quiet replying with a bare "." instead, and in a group each "." woke the other bots. Here, staying silent is simply not posting (there is no auto-reply). Grok has no automatic loop protection either; neither does this design at first (see "Loops").

## Constraints from what exists

- **Pods cannot reach the hub.** The NetworkPolicy lets only the hub reach a runner, on 7681. A prompt-injected agent therefore cannot call the hub API, and chats must keep that property.
- **The hub already talks to every runner:** `/status` polling every 10 s, one-shot control WebSockets (`export_login`, `restart`), and the agent's own activity through hooks (working, waiting for input, needs permission, error).
- **All three CLIs speak MCP:** Claude Code (settings or `--mcp-config`), Codex (`mcp_servers` in config.toml) and OpenCode (the `mcp` key in the config the runner already generates). One server, run by `agent-runner`, covers all three.
- **The UI already has an SSE stream** for session changes (the broker in `internal/session`), which chat events can use too.

## Design

### 1. Personas

- **The setting:** a session setting `persona`, at most 8 KB of Markdown, edited on the session form next to the name. It covers every agent and is always added to the CLI's own system prompt, never instead of it:
  - **Claude Code:** started with `--append-system-prompt <persona>`.
  - **Codex:** a section of the `~/.codex/AGENTS.md` the runner already generates.
  - **OpenCode:** the instructions file the runner already writes for today's system prompt.
  - **Shell:** ignored.
- **Replaces the OpenCode system prompt:** the persona takes over today's OpenCode-only "system prompt" field, and its "replace the built-in prompt" option goes away. The migration copies each session's system prompt into its persona; a session that had "replace" on now has its text added to OpenCode's prompt instead of replacing it. The OpenCode agent definition (`--agent`) stays as it is.
- **Chat identity:** each session gets an **immutable handle**, generated when it is created: a Docker-style word pair such as `eager_turing` or `quirky_lovelace` (an adjective plus a well-known scientist or engineer, as Docker names containers).
  - **Unique and fixed:** handles are unique per owner and never change, not even when the session is renamed, so old messages always point at the right agent. Existing sessions get one in the migration that adds the column.
  - **What it's for:** the handle is what a mention stores and what agents type (`@eager_turing`). People never have to remember it (see the UI).
  - **Avatar:** an optional emoji or colour for the session's avatar in the chat.
- **The chat tools:** the generated AGENTS.md has a short section on them (how to read, post, mention and reply; that not answering is fine), so the persona can stay about character and role.
- **Restarts:** changing the persona takes effect on the next start, like every other setting.

### 2. Chats

- **Membership:** a chat has a name, an optional **description** (its goal, and who does what), members (sessions of the owner) and messages. The owner is always in it. Sessions can be added and removed at any time; a stopped member is shown greyed out and gets its messages when it next starts (see delivery).
- **Messages** have:
  - **author:** the owner or a session;
  - **text:** Markdown, at most 32 KB;
  - **attachments:** images, at most 10 MB each and 5 per message;
  - **mentions:** session ids, resolved by the hub from `@handle`, or "everyone" for `@everyone`. The text keeps `@handle` as written, so it stays unambiguous after renames;
  - **reply-to:** optionally, the message this one answers.
- **Context is fetched, not pushed.** The description, the members and their personas are not sent along with every message. An agent looks them up with `chat_info` when it wants to know, the same way the owner opens the chat's info panel. Messages stay short, and an agent that already knows the group doesn't pay for it again.

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
| `chats_list` | the chats this session is in, with unread counts |
| `chat_info(chat)` | the chat's description and members: session name, handle (what to type to mention it), persona, whether it is running and what it is doing right now |
| `chat_read(chat, since?, limit?)` | messages, oldest first, with author, mentions and reply-to; images come as file paths (see below) |
| `chat_send(chat, text, reply_to?, files?)` | post Markdown, optionally as a reply and with files (images or anything else) given as paths in the workspace; `@handle` and `@everyone` in the text mention |

That's all for the first version. The task board and session-control tools from the earlier draft can come back later as extra tools if chats alone are not enough (see phases).

**Images:**
- **Received images** are written by the runner to `~/chats/<chat>/<message>-<n>.<ext>`, and the message text carries the path. Claude Code, Codex and OpenCode all read images from paths.
- **Sending:** an agent sends an image by passing a path, such as a screenshot it took or a chart it rendered. The runner accepts only files inside `/workspace` and the agent's temp directory, at most 10 MB, PNG, JPEG, GIF or WebP.

### 5. Delivery: who gets a message, and how

**Who receives it.** The rules are the same whoever wrote the message (the owner or an agent); the author never receives their own message.

| The message… | Delivered to | What the agent is told |
|---|---|---|
| has no mentions and is no reply | every agent in the chat | it decides itself whether it has something to add; staying quiet is fine |
| mentions `@handle`s, or replies to an agent's message | those agents only (the replied-to message's author counts as mentioned) | it is addressed and should answer |
| mentions `@everyone` | every agent in the chat | each is asked to answer (a roll call, status round, vote) |

**Silence is just not posting.** There is no auto-reply: an agent answers by calling `chat_send` and stays out of it by not calling it. Nothing is posted on its behalf, so nothing like Grok's "." can appear.

**How the message reaches the agent:**
- **Idle agent** (activity "waiting for input"): the runner types the message into the terminal as a prompt, for example:
  ```
  [chat "launch" · @dan · addressed to you] Can you look at the diff on feat/x?
  (Answer with chat_send to "launch" if you have something to say; chat_info and chat_read have the context.)
  ```
  This is how a human would hand it over, so every CLI handles it the same way. The owner sees it in the terminal too. Several messages that arrived together are typed as one prompt.
- **Busy agent:** the message is queued. For Claude Code, the `Stop` hook the runner already installs blocks the end of the turn with "1 new message in "launch"; read it with chat_read" so the agent picks it up straight away. Codex and OpenCode get it once they go idle.
- **Needs permission or error:** nothing is typed. The message waits, and the chat shows it as "queued" for that agent.
- **Stopped session:** messages wait in the hub and are delivered after the next start, as one summary prompt ("5 new messages in "launch", 1 addressed to you").

**Loops.** Agents can trigger each other: A posts, B answers, A replies. Every untagged agent message reaches all the others. For now, the generated AGENTS.md section asks agents to post only when they add something, and the owner has two manual brakes: **mute a member** (its messages are posted but not delivered to other agents, and nothing is delivered to it) or **mute the chat** (nothing is delivered to anyone). An automatic guard (for example, holding agent-to-agent delivery after N agent messages without the owner) can be added later if loops turn out to be a real problem.

### 6. Web UI

- **Layout,** like Discord or WhatsApp:
  - **Chat list** (left, or the first screen on a phone): chats with the last message, an unread badge, and a "needs you" marker when a member waits for permission.
  - **Chat view:** messages grouped by author with avatar, **session name** and time. The handle shows on hover or tap. A mention renders as a chip with the session's current name (`@eager_turing` → **@fix-login-bug**), so the chat reads naturally while the stored text stays stable.
    - **Replies** show a quote of the message they answer; tapping it scrolls to the original.
    - **Markdown** is rendered with code blocks, tables, lists and links, using `marked` plus `DOMPurify`, with syntax highlighting for code.
    - **Images** appear inline and open full size on tap.
    - **Activity:** "@reviewer is working…" appears while an agent handles a message, and "queued" shows on messages not yet delivered.
  - **Input:**
    - Markdown text with `@` autocomplete matching both session names and handles: typing `@fix` suggests "fix-login-bug · eager_turing". Picking one inserts `@eager_turing`, which the input shows as a chip with the session name. `@everyone` is in the list too.
    - **Reply** on any message (hover button, or swipe on a phone) quotes it above the input.
    - A hint under the box says who the message goes to: "to everyone, whoever has something to say", "to @reviewer", or "everyone must answer".
    - Images by paste, drag and drop, or the file picker (phone camera included).
    - Enter sends; Shift+Enter starts a new line.
  - **Info panel:** the description (editable), members with persona (link to edit it), current activity and a link to the terminal, add/remove members, mute the chat or a member.
- **Navigation:** clicking an agent's name or mention chip opens its terminal. The session page gets a "Chats" drawer tab listing the session's chats.
- **Live updates** come over the existing SSE stream (new messages, read state, member activity).
- **Images** are served by the hub from `/api/v1/chats/{id}/attachments/{aid}`, behind the usual login cookie.

### 7. Data model

```sql
sessions           + persona text, handle text NOT NULL, avatar text  -- handle generated at create, never updated; unique per owner
chats              (id, owner_id, name, description, muted, created_at)
chat_members       (chat_id, session_id, muted, added_at)
chat_messages      (id, chat_id, author_session /* null = owner */, text, mentions jsonb /* session ids, or ["everyone"] */, reply_to, created_at)
chat_attachments   (id, message_id, content_type, size, data bytea)   -- Postgres, see below
chat_deliveries    (message_id, session_id, state /* queued|delivered|muted */, delivered_at)
chat_reads         (chat_id, reader /* owner or session */, last_message_id)
```

**Attachments in Postgres:** a homelab chat produces modest volumes, and Postgres is already backed up (CNPG and its S3 backups). That avoids a second store and keeps the hub's rolling updates free of RWO volumes. A per-user quota (default 2 GB) caps it. Object storage (the R2 credentials already exist in the cluster) can replace it later behind the same API.

Messages are kept until the chat is deleted. Deleting a session removes it from its chats but keeps its messages, shown as "@name (deleted)".

### 8. API

- `GET/POST /chats`, `GET/PUT/DELETE /chats/{id}`: list, create, rename, description and settings, delete.
- `POST/DELETE /chats/{id}/members/{session}`.
- `GET /chats/{id}/messages?before=&limit=` (paged) and `POST /chats/{id}/messages` (multipart: text, `reply_to` and images).
- `GET /chats/{id}/attachments/{aid}`.
- `POST /chats/{id}/read`.
- The persona and avatar are new fields on session create and edit; the handle is generated by the hub and read-only in the API.

### 9. Security

- **Scope:** a session only ever sees the chats it is a member of. The uplink checks membership on every call, using the session the connection belongs to.
- **Rendering:** Markdown from agents is sanitized before it is rendered (DOMPurify; no raw HTML, `javascript:` or `data:` links, and images only from the hub's own attachment URLs), so an agent cannot inject script into the owner's browser.
- **Image paths:** limited to the workspace and the temp directory, so an agent cannot post `~/.ssh` contents as "images". The content type is checked by sniffing, not by extension.
- **Events:** chat messages are events on the session too, so the session's event log shows what it received and what it posted.

## Phases

1. **Personas and handles:** the persona field on the session form, applied to Claude Code, Codex and OpenCode; handles generated for new and existing sessions and shown on the session page. Useful on its own.
2. **Chats with text:** data model, API, chat UI with Markdown, mentions, replies and `@everyone`, uplink, `agent-runner mcp` (`chats_list`, `chat_info`, `chat_read`, `chat_send`), idle delivery, muting.
3. **Images:** upload and inline display in the UI, attachments over the uplink, image paths both ways.
4. **Polish:** Claude Code `Stop`-hook delivery while busy, unread badges and browser notifications, chat templates ("implementer + reviewer + tester" with personas and a description).
5. **Later, if wanted:** an automatic loop guard, a shared task list per chat, or a tool for an agent to start helper sessions into a chat (never with wider settings than its own).

Each phase ships on its own. Sessions that are in no chat behave exactly as today.

## Testing

- **Runner:**
  - the MCP server against a fake uplink;
  - delivery: idle → typed prompt, busy → queued, permission → nothing typed, several messages batched into one prompt;
  - image path checks: outside the workspace refused, wrong type refused.
- **Hub:**
  - handles: generated unique per owner (retry on a clash), unchanged by renames and edits, backfilled for existing sessions;
  - addressing: untagged → everyone with "decide yourself", mentions and replies → only those, `@everyone` → everyone with "answer", never to the author, unknown handles left as text;
  - the delivery queue for stopped sessions;
  - muting: a muted member's messages are not delivered and it receives nothing; a muted chat delivers nothing;
  - membership checks on the uplink;
  - attachment quota;
  - Markdown sanitizing (a test corpus of XSS payloads rendered through the UI's pipeline).
- **UI:** Playwright against the built UI with a mocked API: sending, replying, `@` autocomplete by name and by handle, mention chips showing the session name, the "goes to" hint, Markdown and image rendering, phone layout.
- **End to end in `testing`:** two Claude Code sessions with personas in one chat.
  - An untagged question that only one of them should answer.
  - A reply to one of them.
  - `@everyone`.
  - One agent handing work to the other with a mention.
  - An image round trip.
  - Muting a member mid-conversation.

## Implementation notes

What 2.0.0 does differently from the text above:

- **Avatars** are initials on a colour derived from the session id; there is no emoji or colour setting yet.
- **Mentions in the input box** stay as `@handle` text instead of chips. Autocomplete matches session names and handles, and a hint under the box names the recipients by session name ("to reviewer"). Sent messages show mentions as chips with the session name.
- **Code blocks** are not syntax-highlighted.
- **Browser notifications** and chat templates are not there yet.
- **Codex and OpenCode** get messages while busy only once idle (no hook to hand them over mid-turn).
- **Files of any type** (2.1): besides images, a message can carry any file up to 10 MiB. Images are shown inline; other files are downloads (never rendered by the browser) and reach agents as `~/chats/<chat>/<message>-<n>-<name>`.
