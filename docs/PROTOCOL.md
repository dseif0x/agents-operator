# Terminal protocol

The same WebSocket protocol is spoken between browser and hub, and between hub and runner. Binary frames carry raw PTY bytes with no framing, text frames carry small JSON control messages. There is no base64 and no JSON wrapping of terminal data.

## Endpoints

| Hop | URL | Auth |
| --- | --- | --- |
| browser → hub | `GET /api/v1/sessions/{id}/ws` | session cookie; `Origin` must match an allowed host |
| hub → runner | `ws://<podIP>:7681/ws` | `Authorization: Bearer <RUNNER_TOKEN>` |

The runner also serves, with the same bearer token:

- `GET /healthz` — process alive (no auth; used by the readiness probe)
- `GET /status` — JSON status document (below)
- `GET /scrollback` — the raw ring buffer as `text/plain`

The hub exposes `GET /api/v1/sessions/{id}/scrollback` which proxies the latter.

## Frames

| Direction | Frame | Payload |
| --- | --- | --- |
| both | binary | raw PTY bytes |
| client → server | text | `{"t":"resize","cols":220,"rows":50}` |
| client → server | text | `{"t":"restart"}` — relaunch the agent CLI in the same pod |
| hub → runner only | text | `{"t":"export_login","kind":"claude_login"}` — ask for the CLI's credential file |
| server → client | text | `{"t":"hello","scrollback":true,"cols":220,"rows":50}` — first frame, before the replay |
| server → client | text | `{"t":"exit","code":0}` — the agent process exited; the pod stays up |
| runner → hub only | text | `{"t":"login","kind":"claude_login","data":"<base64>"}` — reply to `export_login` |
| server → client | text | `{"t":"error","message":"..."}` |
| both | ping/pong | every 20 s; a peer is dropped after 3 misses |

The hub forwards binary frames untouched in both directions. From the browser it forwards only `resize` and `restart` control messages and silently drops anything else, so a browser can never ask a runner for a credential file.

## Attach sequence

1. Client connects. Server sends `hello`. `scrollback` is true when a replay follows.
2. Server sends the whole ring buffer (up to 2 MiB) as one binary frame. xterm.js replays it and the TUI redraws correctly because the bytes are the original escape sequences.
3. If the agent has already exited, the server sends `exit` immediately.
4. Live output follows as binary frames. Input from every attached client is written to the PTY; all clients receive all output.
5. Slow clients miss chunks rather than stalling the PTY; a reconnect gets a fresh replay.

The PTY defaults to 220×50 and follows the most recent `resize` from any client.

## Status document (`GET /status`)

```json
{
  "agent": "claude",
  "agent_pid": 12,
  "running": true,
  "exit_code": null,
  "restarts": 0,
  "started_at": "2026-09-28T10:00:00Z",
  "last_output_at": "2026-09-28T10:05:12Z",
  "bytes_since_attach": 1234,
  "cols": 220,
  "rows": 50,
  "clients": 1,
  "needs_attention": true,
  "tail": "Do you want to proceed? (y/n)"
}
```

The hub polls it every 10 s. `needs_attention` is true when the agent has been silent for at least 2 s and the last visible line ends in something that looks like a prompt (`?`, `(y/n)`, `>`, `❯`, `$`, `:` …). `tail` is the last visible line with ANSI stripped, at most 200 characters.

## Runner environment

All of these are injected by the hub; see `internal/runner/protocol.go`.

| Variable | Source | Meaning |
| --- | --- | --- |
| `RUNNER_TOKEN` | per-session Secret | bearer token, rotated on every start |
| `AGENT`, `AUTONOMOUS` | pod spec | which CLI to run; `claude` gets `--dangerously-skip-permissions` when true |
| `REPO_URL`, `REPO_BRANCH` | pod spec | cloned into `/workspace/repo` on first boot |
| `AGENTS_OPERATOR_SESSION` | pod spec | session name |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_BASE_URL` | per-session Secret (from the user Secret) | model credentials |
| `GIT_USER_NAME`, `GIT_USER_EMAIL` | per-session Secret | seeded into `~/.gitconfig` |
| `GIT_SSH_KEY` | per-session Secret | written to `~/.ssh/id_ed25519` (0600); removed from the agent's env |
| `GIT_HTTPS_TOKEN` | per-session Secret | served by `agent-runner git-credential`, never written to disk |
| `AGENTS_OPERATOR_LOGIN_<KIND>` | per-session Secret | base64 credential file seeded into HOME on first boot |
