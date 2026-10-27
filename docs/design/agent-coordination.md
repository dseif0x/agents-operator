# Agent coordination (plan)

Status: proposal, nothing implemented. It builds on the accounts work (#64) and single attach (#62).

## What it is for

Today every session is an island. A user who wants three agents on one change (say an implementer, a reviewer and someone writing tests) has to carry text between their terminals by hand. Coordination lets sessions of the same owner:

- **talk:** send each other short messages, and the human joins the same conversation;
- **share a task list:** take tasks from it and report results;
- **delegate:** a coordinating session may start, watch and stop helper sessions, within limits the owner set.

**Non-goals:**
- **No shared filesystem.** Work moves through git branches, as it already does between a session and its owner.
- **No coordination across users.**
- **No agent framework inside the pod.** The CLIs (Claude Code, Codex, OpenCode) stay unmodified and use the feature through MCP tools.

## Constraints from what exists

- **Pods cannot reach the hub.** The NetworkPolicy lets only the hub reach a runner, and only on 7681. A runner never opens a connection to the hub. That is an important property: a prompt-injected agent cannot call the hub API. Coordination must keep it.
- **The hub already talks to every runner:** `/status` polling every 10 s, plus one-shot control WebSockets (`export_login`, `restart`). The runner already gets the agent's own account of what it is doing through hooks (`Activity`: working, waiting for input, needs permission, error, plus the last message).
- **All three CLIs speak MCP:** Claude Code (`--mcp-config` or settings), Codex (`mcp_servers` in config.toml) and OpenCode (the `mcp` key in the config the runner already generates). One MCP server, run by `agent-runner`, covers all three.

## Design

### 1. Uplink: runner → hub requests over a hub-opened WebSocket

For each running session with coordination on, the hub keeps one long-lived WebSocket to the runner: `/ws?uplink=1`, authenticated with the runner token like the other control connections. Requests flow from runner to hub over it, and answers come back. The hub opens the connection, so the network direction stays hub → pod.

- **Framing:** JSON `{"id": n, "op": "...", "args": {...}}`, answered with `{"id": n, "ok": true, "result": ...}` or `{"id": n, "ok": false, "error": "..."}`. The hub also pushes events on it (`{"event": "message", ...}`) so the runner learns about new messages without polling.
- **Reconnects:** if the connection drops, the hub reopens it on the next poll, within 10 s. Runner-side calls made meanwhile wait up to 30 s, then fail with a clear error the tool shows the agent.
- **Leadership:** only the leader hub holds uplinks, as only it acts on side effects today.

### 2. `agent-runner mcp`: the tools the agent sees

`agent-runner mcp` is a stdio MCP server that the runner registers with the session's CLI (Claude Code settings, Codex config, OpenCode config). It forwards tool calls to the runner over a local socket, and the runner forwards them over the uplink. Tools:

| Tool | What it does |
|---|---|
| `sessions_list` | the owner's sessions in this crew: name, agent, state, activity, repos, branch hints |
| `message_send(to, text)` | message a session by name, `@crew` (everyone), or `@owner` (the human) |
| `messages_read(since?)` | messages addressed to this session or the crew, oldest first |
| `task_create(title, body, assignee?)` / `task_list(state?)` / `task_claim(id)` / `task_update(id, state, result?)` | the crew's task list |
| `session_start(name, agent, repos, prompt, account?)` | coordinator only: start a helper session in the crew with an initial prompt |
| `session_status(name)` / `session_tail(name, lines)` | coordinator only: a helper's activity and the end of its screen text |
| `session_stop(name)` / `session_send(name, text)` | coordinator only: stop a helper, or type text into its terminal as if the owner did |
| `wait_for(names, until)` | block (max 10 min) until helpers are idle, finished or need attention; a coordinator uses it instead of polling |

Message text is limited to 16 KB and task bodies to 64 KB. Anything bigger goes through git: "pushed `feat/x`, see `docs/notes.md`".

### 3. Getting messages to an agent

An agent that is working only sees a message when it calls `messages_read`. That is not enough, so the runner also delivers messages itself:

- **Idle agent** (activity "waiting for input"): the runner types the message into the PTY as a user prompt, prefixed `[from reviewer]`. This is how a human would hand it over, so every CLI handles it the same way.
- **Busy Claude Code:** the `Stop` hook the runner already installs gets a new answer. When unread messages exist at the end of a turn, it blocks the stop with "N new messages; read them with messages_read", so the agent continues instead of going idle. Codex and OpenCode get the idle delivery only until they have equivalent hooks.
- **Needs permission or error:** the runner does not interrupt. The message waits, and the UI shows it as pending.

Each delivery path can be turned off per session ("tools only").

### 4. Crews

A **crew** is a named group of the owner's sessions that see each other. A session belongs to at most one crew. The owner creates a crew in the UI and adds sessions, or a coordinator's `session_start` puts the helper into its own crew. Sessions outside a crew have no uplink and no MCP server, so nothing changes for them.

A crew has one optional **coordinator** session. Only the coordinator gets the `session_*` tools, and only for sessions of its crew.

### 5. Limits (who may do what)

Delegation must never be a way around the settings the owner chose:

- **Settings:** a helper started by a coordinator gets at most the coordinator's settings. That covers its Kubernetes access mode and namespaces (or none), egress, the git credentials switch, the resource ceiling, and the runtime class. The coordinator can narrow them but not widen them. The AI account follows the accounts rules (#64): the owner's default unless the coordinator names one of the owner's accounts.
- **Crew limits** set by the owner: maximum helpers alive (default 3), maximum helper lifetime (default 4 h), and whether helpers may be autonomous.
- **Visibility:** every coordination action is an event on both sessions (`coordination: reviewer started by lead`) and appears in the crew timeline.
- **Kill switch:** the owner can pause a crew, which drops every uplink and makes the tools answer "paused by the owner".

### 6. Data model

```sql
crews          (id, owner_id, name, coordinator_session, max_helpers, max_helper_age, allow_autonomous, paused, created_at)
crew_members   (crew_id, session_id, role /* member|coordinator|helper */, started_by)
crew_messages  (id, crew_id, from_session /* null = owner */, to_session /* null = crew */, text, created_at)
crew_reads     (session_id, last_message_id)
crew_tasks     (id, crew_id, title, body, state /* open|claimed|blocked|done */, assignee, result, created_by, updated_at)
```

Messages are kept for the crew's lifetime and capped at 2,000 per crew (the oldest are pruned, like events).

### 7. UI

- **Crew page:** members as cards (live activity, needs-attention badge), the message timeline with an input to post as the owner, the task board (open, claimed, done), and pause/resume.
- **Session page:** a "Crew" drawer tab with the timeline, so the owner can watch the conversation next to the terminal.
- **Session list:** groups crews together; a crew needing attention bubbles up like a session does today.

## Phases

1. **Messages:** crews, uplink, `agent-runner mcp` with `sessions_list`, `message_send` and `messages_read`, idle delivery, the crew timeline in the UI. Already useful: two sessions that can hand work to each other.
2. **Tasks:** the task tools and board.
3. **Coordinator:** `session_start/status/tail/stop/send`, `wait_for`, limits, and the Claude Code Stop-hook delivery.
4. **Polish:** the crew page, templates ("implementer + reviewer"), and a prompt snippet the coordinator gets in AGENTS.md explaining the tools and limits.

Each phase ships on its own and is off for sessions outside a crew.

## Testing

- **Runner:** the MCP server against a fake uplink. Delivery: idle → PTY input; working → nothing typed; the Stop-hook answer when unread messages exist.
- **Hub:** uplink reconnects and leader handover; crew limits (helper count, settings ceiling, paused crew); message pruning.
- **End to end in `testing`:** two shell sessions in a crew exchanging messages through `agent-runner mcp` called by hand. Then a Claude Code coordinator starting a helper with a tiny task and waiting for it.

## Open questions

1. **Coordinator scope:** should a coordinator be able to start sessions on *different* repositories than its own, or only on the crew's repos?
2. **Shared branch:** should helpers get a shared branch convention by default (`crew/<name>/<session>`), or is that left to the prompts?
3. **Accounts for helpers:** is "the coordinator may pick any of the owner's accounts for a helper" fine, or should the owner pre-select which accounts a crew may use (for example to keep paid API keys out of helpers)?
4. **Sandbox mode:** should helpers of a sandboxed coordinator share its vcluster (one sandbox per crew) or get their own?
