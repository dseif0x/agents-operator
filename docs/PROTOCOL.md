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
2. Server sends the whole ring buffer (up to 2 MiB) as one binary frame. xterm.js replays it and the TUI redraws correctly because the bytes are the original escape sequences. When the agent is on the alternate screen and the switch to it is no longer in the buffer, the frame starts with `ESC[?1049h`.
   Then, when the agent has changed any of them, a second binary frame sets the terminal modes it is in now (mouse reporting and encoding, bracketed paste, cursor visibility, cursor keys, focus events): full-screen TUIs set these once at startup, long out of the buffer, and a fresh xterm.js would otherwise not report clicks.
   A full-screen TUI (one on the alternate screen) that draws only what changed does not repaint the rest for a new client. When a client's first `resize` keeps the PTY's size, the runner makes the PTY one row shorter for 250 ms and then restores it, so the TUI repaints the whole screen. A resize that changes the size makes it repaint anyway.
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

The hub polls it every 10 s. `tail` is the last visible line with ANSI stripped, at most 200 characters. Without hook events, `needs_attention` is true when the agent has been silent for at least 2 s and the tail ends in something that looks like a prompt (`?`, `(y/n)`, `>`, `❯`, `$`, `:` …).

With hook events (see below) the document also carries the agent's own account, and `needs_attention` is derived from it instead:

```json
"activity": {
  "state": "tool",
  "detail": "Bash: go test ./...",
  "message": "I fixed the bug in api.go and added a test.",
  "since": "2026-10-03T10:05:12Z"
}
```

| `state` | Meaning | `needs_attention` |
| --- | --- | --- |
| `thinking` | a turn is running, no tool at the moment | no |
| `tool` | a tool call runs; `detail` is `Tool: target` (Bash description or command, file name, pattern, URL, query) | no |
| `needs_permission` | blocked on a permission or MCP prompt; `detail` says which | yes |
| `waiting_input` | the turn ended (`message` is the last reply, trimmed to 300 characters), the CLI just started (`detail: ready`) or has been idle (`detail: idle`) | yes |
| `error` | the turn failed; `detail` is the error type (`rate_limit`, `authentication_failed` …) | yes |
| `exited` | the CLI session ended | no |

## Agent hooks

Claude Code fires a hook on every lifecycle event, and the runner installs one for the events it cares about in `~/.claude/settings.json` on boot (`SessionStart`, `SessionEnd`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `PermissionRequest`, `Notification`, `Stop`, `StopFailure`, `Elicitation`). Each is a `command` hook running `agent-runner hook`, marked `async` so the CLI neither waits for it nor can be blocked by it; the helper reads the event JSON from stdin, posts it to the runner at `http://127.0.0.1:7682/hook` and exits 0 whatever happens. Hook groups the user added to the same file are kept; the runner's is recognised by its command and added once per event. Events from subagents (`agent_id` set) are ignored so a Task's inner tool calls do not flip the main agent's state. The listener is loopback only: nothing outside the pod can post to it, and nothing posted to it changes what the CLI does.

Codex and OpenCode have no hooks installed yet and fall back to the tail heuristic. `login_updated_at` is present only when the agent CLI rewrote its credential file after boot (a login or a token refresh; files seeded by the runner do not count); if the owner has that login saved and the account copy is older, the hub sends `export_login` and replaces it.

## Runner environment

All of these are injected by the hub; see `internal/runner/protocol.go`.

| Variable | Source | Meaning |
| --- | --- | --- |
| `RUNNER_TOKEN` | per-session Secret | bearer token, rotated on every start; runner-only (see below) |
| `AGENT`, `AUTONOMOUS` | pod spec | which CLI to run; `claude` gets `--dangerously-skip-permissions` when true |
| `AGENT_GIT_CREDENTIALS` | pod spec | `false` keeps `AGENTS_OPERATOR_GIT_SSH`, `AGENTS_OPERATOR_GIT_HTTPS` and `GH_TOKEN` runner-only: used for the first-boot clones, never given to the agent; absent means `true` |
| `REPOS` | pod spec | JSON list of `{url, branch, path}`; each is cloned into `/workspace/<path>` on first boot; the agent starts in `/workspace` |
| `AGENTS_OPERATOR_SESSION` | pod spec | session name |
| `K8S_ACCESS`, `K8S_NAMESPACES` | pod spec | what kubectl in the pod may do (`off`, `readonly`, `namespace`, `sandbox`) and, in namespace mode, the comma-separated namespaces with write access; only used for the generated AGENTS.md |
| `SANDBOX_NAMESPACE`, `SANDBOX_HOST`, `SANDBOX_POD_SECURITY` | pod spec | sandbox mode: the vcluster's host namespace, the DNS name its Ingress hosts live under (empty without a sandbox domain) and the Pod Security level enforced there; only used for the generated AGENTS.md. `KUBECONFIG` points at the mounted kubeconfig |
| `EGRESS` | pod spec | comma-separated local network destinations the session's NetworkPolicy opens; only used for the generated AGENTS.md |
| `ANTHROPIC_API_KEY` (+ `ANTHROPIC_BASE_URL`), `CLAUDE_CODE_OAUTH_TOKEN`, `OPENAI_API_KEY` | per-session Secret (from the owner's accounts) | the one account a `claude` or `codex` session signs in with (the session's `ai_account`, else the owner's default for the agent, else the oldest it can use); a `shell` session gets the default of each; read by the CLIs themselves |
| `AGENTS_OPERATOR_AI_ACCOUNTS` | per-session Secret | `opencode` and `shell`: JSON list of `{id, name, kind, base_url, models, key_env}`, one per AI account the session gets (`opencode`: Anthropic and OpenAI keys, OpenAI-compatible endpoints, Unjailed; `shell`: every account with a key); no values |
| `AGENTS_OPERATOR_KEY_<NAME>` | per-session Secret | the key of each account in that list, named after the account (`Work LiteLLM` → `AGENTS_OPERATOR_KEY_WORK_LITELLM`) |
| `GIT_USER_NAME`, `GIT_USER_EMAIL` | per-session Secret | the profile's git identity, seeded into `~/.gitconfig` |
| `AGENTS_OPERATOR_GIT_SSH` | per-session Secret | JSON list of `{host, key}`, one per SSH git account; written to `~/.ssh/agents-operator/key-<n>` (0600) with an ssh config of one `Host` block per key (`*` = every host no other key names) that git uses through `GIT_SSH_COMMAND=ssh -F ~/.ssh/agents-operator/config`; runner-only |
| `AGENTS_OPERATOR_GIT_HTTPS` | per-session Secret | JSON list of `{host, username, token}`, one per HTTPS git account; `agent-runner git-credential` answers git with the account naming the host, then a matching pattern, then `*`; never written to disk |
| `GH_TOKEN` | per-session Secret | the github.com HTTPS account's token, for `gh` (also exported as `GITHUB_TOKEN`); without an SSH key for github.com `git@github.com:` remotes are rewritten to HTTPS |
| `AGENTS_OPERATOR_LOGIN_<KIND>` | per-session Secret | base64 login bundle of the saved login account the session signs in with, seeded into HOME; only the files known for that kind are written, and never over an existing file unless the session switched accounts; runner-only |
| `AGENTS_OPERATOR_SEED_ACCOUNT_<KIND>` | per-session Secret | the id of that account; the runner remembers it in `~/.config/agents-operator/login-<kind>` and re-seeds when it changes |
| `AGENTS_OPERATOR_PRIVATE_FILE` | `agent-runner init` | where init put the runner-only variables; the runner reads and deletes the file at start |

The runner-only variables (and, with `AGENT_GIT_CREDENTIALS=false`, the git tokens) never reach the agent. `agent-runner init`, the image's entrypoint, moves them into a fresh 0600 file under `/tmp` and execs tini without them, so `/proc/1/environ` does not hold them; the runner loads and deletes the file before the agent starts, leaves them out of the agent's environment and marks itself non-dumpable (`PR_SET_DUMPABLE` 0), which closes its `/proc` entries and ptrace to the agent, a process of the same user without capabilities.

For `AGENT=claude` the runner also prepares `~/.claude.json` before the CLI starts: with any credential present (seeded login, `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`) onboarding is marked complete, an API key from the environment is pre-approved, and `/workspace` plus every repository directory are marked trusted. With `AUTONOMOUS=true` the consent behind `--dangerously-skip-permissions` is recorded in `~/.claude/settings.json` (`skipDangerousModePermissionPrompt`). Keys that already exist are left as they are.

For `AGENT=opencode` the runner passes OpenCode its session settings in `OPENCODE_CONFIG_CONTENT`, which OpenCode merges over its own config files, and starts it with `--auto` (approve what is not explicitly denied) when `AUTONOMOUS=true`. Each account in `AGENTS_OPERATOR_AI_ACCOUNTS` becomes a provider named after the account, its key referenced from the environment (`{env:AGENTS_OPERATOR_KEY_…}`) rather than written out: the first Anthropic and OpenAI keys configure OpenCode's own `anthropic` and `openai` providers (an Anthropic base URL gets `/v1` appended), further ones become providers of their own (id from the account name) through the AI SDK packages with the models their API lists; an OpenAI-compatible endpoint gets OpenCode's OpenAI-compatible provider with the account's models or, if it lists none, the endpoint's (an endpoint without any is left out). The first Unjailed account is the `unjailed` provider (base URL default `https://unjailedai.com/v1`), with the endpoint's models (fetched from `<base>/models` at every start, a built-in list if that fails) with their context sizes (the 1M, 400K, 256K or 128K unjailedai.com lists for each of its 32 models, 128k for one it adds later; 32k output). Until a model has been picked in OpenCode (its recent models in `~/.local/state/opencode/model.json`), `glm-5.3`, or the endpoint's first model, is the default; after that the pick stays, because a configured model would win over it. `glm-5.3-flash` does titles. OpenCode sends earlier replies' reasoning back with every request, and the GLM models refuse one with over 30,000 characters of it, so the runner also writes a plugin (`~/.config/opencode/agents-operator/unjailed-reasoning.js`, loaded through the configuration's `plugin` list) that leaves the reasoning out of requests to a GLM model of any Unjailed provider; the session keeps it, and DeepSeek, which needs it back, gets it as before. Self-update, sharing and OpenCode's Claude Code compatibility (`~/.claude/CLAUDE.md`, Claude skills) are turned off (`OPENCODE_DISABLE_AUTOUPDATE`, `OPENCODE_DISABLE_SHARE`, `OPENCODE_DISABLE_CLAUDE_CODE`).

An OpenCode session can also carry a custom system prompt and an agent definition (session fields `system_prompt`, `system_prompt_replace`, `agent_definition`; refused for other agents). They reach the pod as `AGENT_SYSTEM_PROMPT` with `AGENT_SYSTEM_PROMPT_MODE` (`append` or `replace`) and `AGENT_DEFINITION` with `AGENT_DEFINITION_NAME`, and the runner turns them into OpenCode configuration on every start: a replacing prompt becomes the prompt of OpenCode's default `build` agent, an appended one an `instructions` file (`~/.config/opencode/agents-operator/system-prompt.md`). The definition, Markdown with front matter (`name`, `description`, optionally OpenCode agent settings such as `model`, `temperature` or `permission`, then the prompt), becomes a primary OpenCode agent selected with `--agent <name>`; its prompt replaces OpenCode's, so alongside it the system prompt can only be appended. Neither is left in the CLI's environment.

The agent's `TMPDIR` is a private `0700` directory under the pod's `/tmp`. Claude Code keeps its cross-session messaging socket under `/tmp` and vets every ancestor of that directory: a world-writable directory without the sticky bit is refused with a warning on every start, and that is exactly what Kubernetes makes of an emptyDir (a PVC root with `fsGroup` is group-writable and refused as well; `XDG_RUNTIME_DIR`, `CLAUDE_CODE_TMPDIR` and `--messaging-socket-path` under either mount fail the same check). Since the non-root runner cannot change the mount's mode, the pod runs a `tmp-sticky` init container as root that does `chmod 1777 /tmp` before the runner starts (`runner.tmpInit`, on by default; see OPERATIONS.md).
