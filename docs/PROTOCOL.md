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
| runner → hub only | text | `{"t":"login","kind":"claude_login","data":"<base64>"}` — reply to `export_login`; `data` is a JSON bundle `{"files":{"<HOME-relative path>":<bytes>}}` with every file that makes up the login (for Claude Code: `.claude/.credentials.json` and `.claude.json`) |
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
  "tail": "Do you want to proceed? (y/n)",
  "login_updated_at": "2026-09-28T10:04:00Z"
}
```

The hub polls it every 10 s. `needs_attention` is true when the agent has been silent for at least 2 s and the last visible line ends in something that looks like a prompt (`?`, `(y/n)`, `>`, `❯`, `$`, `:` …). `tail` is the last visible line with ANSI stripped, at most 200 characters. `login_updated_at` is present only when the agent CLI rewrote its credential file after boot (a login or a token refresh; files seeded by the runner do not count); if the owner has that login saved and the account copy is older, the hub sends `export_login` and replaces it.

## Runner environment

All of these are injected by the hub; see `internal/runner/protocol.go`.

| Variable | Source | Meaning |
| --- | --- | --- |
| `RUNNER_TOKEN` | per-session Secret | bearer token, rotated on every start |
| `AGENT`, `AUTONOMOUS` | pod spec | which CLI to run; `claude` gets `--dangerously-skip-permissions` when true |
| `REPOS` | pod spec | JSON list of `{url, branch, path}`; each is cloned into `/workspace/<path>` on first boot; the agent starts in `/workspace` |
| `AGENTS_OPERATOR_SESSION` | pod spec | session name |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_BASE_URL` | per-session Secret (from the user Secret) | model credentials |
| `CLAUDE_CODE_OAUTH_TOKEN` | per-session Secret | long-lived Claude subscription token from `claude setup-token`; read by Claude Code itself |
| `GIT_USER_NAME`, `GIT_USER_EMAIL` | per-session Secret | seeded into `~/.gitconfig` |
| `GIT_SSH_KEY` | per-session Secret | written to `~/.ssh/id_ed25519` (0600); removed from the agent's env |
| `GIT_HTTPS_TOKEN` | per-session Secret | served by `agent-runner git-credential` for every host, never written to disk |
| `GH_TOKEN` | per-session Secret | GitHub token for `gh` (also exported as `GITHUB_TOKEN`); the credential helper uses it for `https://github.com` when no `GIT_HTTPS_TOKEN` is set, and without an SSH key `git@github.com:` remotes are rewritten to HTTPS |
| `AGENTS_OPERATOR_LOGIN_<KIND>` | per-session Secret | base64 login bundle seeded into HOME on first boot; only the files known for that kind are written, and never over an existing file |

For `AGENT=claude` the runner also prepares `~/.claude.json` before the CLI starts: with any credential present (seeded login, `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`) onboarding is marked complete, an API key from the environment is pre-approved, and `/workspace` plus every repository directory are marked trusted. Keys that already exist are left as they are.
