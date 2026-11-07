# Conversations: agents without terminals

Status: phase 1 of the OpenBot-style rework (owner's request, 2026-11-07).

## Goal

agents-operator should work like OpenBot (github.com/nightly-labs/openbot):
every agent is a persistent teammate you chat with. Its own chat shows your
messages, its answers, its thinking and its work steps (commands, file
edits, tool calls) as structured items, with a queue, Stop, and later
routines, threads, memories and approvals. There is no terminal in the main
view. Group chats (our "chats", OpenBot's channels) stay as they are and get
the same look.

The terminal stays reachable from a session's menu: CLI logins (`claude
/login`, `codex login`) still need it, and it is the fallback for anything
the structured view does not cover.

Phases (one PR each):

1. **Conversations** (this document): the runner drives the agent CLI
   through its structured protocol; the hub stores the conversation; the UI
   shows it. Queue, Stop, context usage.
2. **Frontend**: the OpenBot layout (rail, sidebar with Needs you / agents /
   chats, tabs, side panel, phone layout) around conversations and chats.
3. **Routines**: schedules and webhooks per agent and per chat.
4. **Agent features**: threads, memories, task lists, files, usage,
   approvals and questions as cards.
5. **Chats like channels**: lead routing, waiting-for-replies, stopped-task
   cards.

## Interface mode

A session has an `interface`: `conversation` (the default for new Claude
Code, Codex and OpenCode sessions) or `terminal` (shell sessions, and older
sessions until they are switched in their settings). The runner reads it
from `AGENT_INTERFACE`. Switching restarts the agent; the workspace and the
CLI's own session files stay.

## Drivers (runner)

In conversation mode the runner starts the CLI as a child process with
pipes instead of a PTY and speaks its protocol:

| Agent | Protocol | Notes |
|---|---|---|
| Claude Code | `claude -p --input-format stream-json --output-format stream-json --verbose --include-partial-messages` | one long-lived process; user messages as JSON lines on stdin; interrupt with a `control_request` (`interrupt`); resume with `--resume <session id>` |
| Codex | `codex app-server` (JSON-RPC 2.0 over stdio) | `initialize`, `thread/start` / `thread/resume`, `turn/start`, `turn/interrupt`; `item/*` and `turn/*` notifications |
| OpenCode | `opencode acp` (Agent Client Protocol, JSON-RPC over stdio) | `initialize`, `session/new` / `session/load`, `session/prompt`, `session/cancel`; `session/update` notifications |

Each driver turns its protocol into the same small event model:

- `turn_started {turn}`: the agent took the next input.
- `item {turn, key, kind, status, text | delta, tool}`: one item of the
  answer, created or updated. Kinds: `text` (the answer), `thinking`, `tool`
  (a command, a file change, an MCP or other tool call: name, a one-line
  summary of the input, the output trimmed to 16 KiB), `error`, `notice`
  (compaction, model change, ...). `delta` appends to the item's text, so
  answers stream.
- `turn_ended {turn, status, usage, error}`: `done`, `interrupted` or
  `failed`; usage is input/output tokens and the context in use.
- `info {model, context_window, ref}`: what the CLI reported at start; `ref`
  is its own session id, which the runner keeps in
  `~/.agents-operator/conversation.json` to resume after a restart.

**Permissions and questions.** A session's Autonomous setting decides
permissions: autonomous sessions grant everything (Claude Code
`--dangerously-skip-permissions`, Codex `approvalPolicy: never` with full
access, ACP requests granted); otherwise each permission becomes a
`request` event and a card in the UI (Allow, Always allow, Deny): Claude
Code's `can_use_tool` control requests (`--permission-prompt-tool stdio`),
Codex's `requestApproval` server requests (sandbox `workspace-write`,
`approvalPolicy: on-request`), ACP's `session/request_permission`.
Questions to the owner (Claude's `AskUserQuestion`, Codex's
`requestUserInput`) are always cards. The answer goes back over the uplink
(`respond`); a request the turn outlives ends as `cancelled`. While one
waits the agent's activity is `needs_permission` (the "Needs you" list and
a push notification).

**Models.** The drivers report the models the owner can pick (Claude
Code's aliases, Codex's `model/list`, ACP's `availableModels`); the header
switches with `set_model` (Claude's control request, Codex's per-turn
model, ACP's `session/set_model`). The choice is kept in the runner's state
file and used on the next start.

**Memories.** Short notes per agent (`memories`), written by the agent's
`memory_save` / `memory_forget` tools or by the owner. The hub pushes them
over the uplink; the runner puts a `[agents-operator memories]` block in
front of the next input whenever they changed, and again after the CLI
restarts.

**Continuing a terminal session.** On the first start without a
conversation id, a Claude Code session resumes the newest transcript in
`~/.claude/projects/<workspace>/` and imports its last 3000 items into the
conversation (source `imported`), so a session switched from the terminal
keeps its context and its history.

The persona, the chat MCP server (`agent-runner mcp`), the context sizes
and the account setup stay exactly as for terminal sessions.

## Inputs and the queue

Everything an agent works on is an **input**: a message from the owner, a
chat delivery (as today: the runner builds the `[agents-operator chat]`
text), later a routine run. The runner keeps one FIFO and hands the next
input to the driver when the agent is idle; Claude Code and Codex get
follow-ups during a turn only through the queue (no steering in phase 1).

Owner messages are stored in the hub first (`conversation_inputs`, state
`queued`) and pushed over the uplink (`{"event":"inputs",...}`), like chat
deliveries. Before the runner sends one to the agent it claims it
(`input_claim`); the hub marks it `sent` only if it is still queued, so an
edit or a cancel in the UI never races the agent. Queued messages can be
edited, reordered later, and cancelled.

## Hub

Tables (migration 00016):

- `conversation_items`: `id bigserial`, `session_id`, `thread` (`main` for
  now), `turn`, `key` (the driver's id for the item), `kind`, `status`,
  `data jsonb` (text, tool fields, attachments, usage), timestamps. A user
  message is an item too (`kind = user`, with the input id and its source).
- `conversation_inputs`: the queue: `id`, `session_id`, `thread`, `text`,
  `attachments`, `source` (`owner`, `chat`, `routine`), `state` (`queued`,
  `sent`, `cancelled`), timestamps.
- `sessions.interface`, and `sessions.conversation` (jsonb: model, context
  window, last usage, running turn) for the list and the header.

The leader keeps an uplink to every running conversation session (not only
those in chats). Runner events arrive as the uplink op `events` (a batch);
the hub applies them, writes text deltas at most once a second per item
(the browser gets every delta live), and publishes `item` / `turn` /
`input` events on the existing SSE feed. Files the agent attaches stay as
today (`chat_send`); files the owner attaches to a message are stored like
chat attachments and written into the pod before the input is sent.

API:

- `GET /api/v1/sessions/{id}/items?before=&limit=` (newest page first)
- `POST /api/v1/sessions/{id}/inputs` (text, attachments) → queued input
- `PATCH /api/v1/sessions/{id}/inputs/{input}` (edit while queued),
  `DELETE` (cancel)
- `POST /api/v1/sessions/{id}/interrupt` (Stop)

## UI (phase 1 part)

The session page shows the conversation: your messages right, the agent's
left, thinking and tool steps folded into a "steps" group per turn
(expandable, each step with its input and output), streaming text, a
working indicator, queued messages above the composer (edit, cancel), Stop,
and the context use in the header. The terminal moves to the menu. Phase 2
builds the full layout around it.

## Tests

- Drivers: unit tests against fake CLIs that replay recorded protocol
  traffic, plus a recorded run of each real CLI.
- End to end: the three CLIs against a small local model (llama.cpp on CPU,
  OpenAI and Anthropic compatible) in the `testing` namespace.
