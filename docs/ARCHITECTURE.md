# Architecture

agents-operator is a self-hosted mission control for AI coding agents. Every session is a Kubernetes Pod with its own PersistentVolumeClaim, and the agent's real terminal is streamed to the browser. There is no chat UI around an SDK: the CLI's own TUI is the interface.

## Components

```
 browser ──HTTPS/WSS──▶ hub (Go, 1 replica, rolled with surge) ──ws://podIP:7681──▶ session pod
                          │  REST API, SSE                       │ agent-runner (PTY owner)
                          │  WebSocket proxy                     │ claude / opencode / codex
                          │  reconciler (Pods, PVCs, Secrets)    │ /workspace  ← PVC
                          ▼                                      │ RUNNER_TOKEN ← Secret
                        Postgres (session registry)
```

| Piece | Runs as | Owns |
| --- | --- | --- |
| **hub** (`cmd/agents-operator`) | Deployment, 1 replica, distroless, leader Lease for rolling updates | REST API, SSE feed, WebSocket terminal proxy, reconciler (leader only), embedded SPA |
| **agent-runner** (`cmd/agent-runner`) | PID 1 in every session pod (under tini) | The PTY, the 2 MiB scrollback ring buffer, the runner WebSocket on `:7681`, the loopback hook endpoint on `127.0.0.1:7682` that turns Claude Code's hook events into the session's activity |
| **Postgres** | Bitnami subchart or external | users, sessions, session_events, user_credentials |

The browser only talks to the hub. The hub reaches runners by pod IP on the cluster network and is the only component that calls the Kubernetes API. Session pods have no Kubernetes identity at all (`automountServiceAccountToken: false`).

## Packages

| Package | Responsibility |
| --- | --- |
| `internal/config` | Typed config from `AGENTS_OPERATOR_*` env; fails fast |
| `internal/store` | Postgres via pgx; goose migrations embedded and applied under an advisory lock; in-memory implementation for tests |
| `internal/auth` | argon2id passwords, HMAC-signed cookie sessions, CSRF tokens, login rate limit, runner token minting; `Authenticator` is the OIDC seam |
| `internal/k8s` | Client (in-cluster, kubeconfig fallback) and label-filtered informers for Pods, PVCs, Secrets |
| `internal/reconcile` | Pure spec builders (`BuildPVC`, `BuildSecret`, `BuildPod`) and the level-triggered converge loop; `Orchestrator` is the seam for a future Sandbox-CRD backend |
| `internal/session` | Create/start/stop/delete, ownership checks, runner status poller, idle policy, SSE broker, per-user credential Secrets, Prometheus metrics |
| `internal/term` | The WebSocket pipe browser ↔ hub ↔ runner; status, scrollback and control helpers |
| `internal/api` | HTTP handlers, middleware (host allowlist, cookie auth, CSRF, security headers), SSE |
| `internal/ui` | `embed.FS` of `internal/ui/dist` behind the `ui` build tag; a 503 stub otherwise |
| `internal/runner` | Protocol types shared by hub and runner; `internal/runner/server` is the runner implementation |

## Session model

A session is one row, one PVC, one Secret and zero or one Pod, all named `agents-operator-<id>` and labelled `agents-operator.io/session=<id>`. The PVC is the identity that survives; the Pod is disposable.

A session can also pick where and how its pod runs: a `runtime_class` (overrides the chart's `runner.runtimeClassName`), a `node_selector` (merged over the chart's, session keys win) and `tolerations` (appended to the chart's). All three are validated with Kubernetes name and label syntax at creation so a typo is a 400 rather than a pod stuck in `creating`; the New Session form has them under "Advanced: scheduling". The same section takes CPU and memory limits and extended resources (`resources.limits["nvidia.com/gpu"] = "1"`, hugepages; whole numbers only, request set equal to the limit as Kubernetes requires). Every limit is the session's value if given, else the chart's `runner.resources` default, capped by `runner.maxResources` (or by the default when the maximum leaves it out), so the operator decides both what a session gets for free and the most anyone may ask for.

Kubernetes access is a per-session mode (`k8s_access`). `off` is the default: no identity at all. `readonly` needs the chart's runner ServiceAccount: the pod gets `serviceAccountName` and a mounted token, `kubectl` in the image works, and the generated AGENTS.md tells the agent it may read but not change the cluster. `namespace` adds write access in namespaces the owner lists (`k8s_namespaces`) and needs the chart's `runner.serviceAccount.namespaceWrite`: because the shared account would hand every session the same rights, the reconciler creates an account per session (`agents-operator-<id>` in the hub namespace), binds it to the read role like the shared one (hub namespace or cluster-wide, as the chart chose) and to the write role (`edit` by default) with one RoleBinding per listed namespace, and only then creates the pod. Those bindings live in other namespaces, out of reach of owner references, so the reconciler removes them itself when the pod is gone (stop, delete), prunes namespaces dropped by an edit on the next start, and sweeps orphans by label like it does for pods. The hub's own namespace and `kube-system` are refused as targets; a namespace that does not exist fails the start with a reason instead of a pod stuck in `creating`. The runner learns the mode through `K8S_ACCESS` and `K8S_NAMESPACES` and tells the agent exactly where it may write.

A stopped or failed session can be edited (`PATCH /api/v1/sessions/{id}`, same body as create): name, repositories, image tag, resources, scheduling, Kubernetes access, environment and autonomy. The agent, PVC size and storage class are fixed because they shape the volume and the CLI state on it. Nothing happens until the next start, which builds a fresh pod from the new settings; repositories added later are cloned then, since the runner only clones into empty directories.

A session lists zero or more repositories (`repos`: url, optional branch, directory name). On first boot the runner clones each into `/workspace/<path>`; the agent starts in `/workspace` itself, next to the generated `AGENTS.md`, with every repository one directory down. Later boots skip directories that are not empty, so work survives stop/start.

On every boot the runner also writes `/workspace/AGENTS.md`: what the pod is, which repositories were cloned where, what tools and credentials (`gh`, SSH key, git identity) are available, and a few conventions. The same content is placed in each CLI's global instructions file (`~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`, `~/.config/opencode/AGENTS.md`) unless the user wrote their own, so Claude Code, Codex and OpenCode all read it without configuration. Per-repository `AGENTS.md`/`CLAUDE.md` files still apply on top.

```
creating ──pod Ready──▶ running ──stop──▶ stopping ──pod gone──▶ stopped
   ▲  │ error/timeout      │ pod gone / crashed                    │ start
   │  ▼                    ▼                                       │
   │ failed ◀──────────────┘                                       │
   └────────────────── start (from stopped or failed) ─────────────┘
 any ──delete──▶ deleting ──all objects gone──▶ row deleted
```

The reconciler never trusts the state column as truth about the cluster. On every pod event and every tick (30 s) it observes what exists through the informer caches and acts:

- **creating**: ensure PVC, ensure Secret for the current *generation* (a fresh runner token per start), remove a stale or crashed pod, create the pod, move to running when Ready; fail after `CreatingTimeout` (15 min).
- **running**: pod missing or terminal → failed (the pod is kept for logs).
- **stopping**: delete the pod; stopped once it is gone. PVC and Secret stay.
- **deleting**: delete all three; delete the row once they are gone.
- **orphans** (labelled objects without a row): deleted after a 2 min grace period and logged.

Starting a stopped or failed session bumps `generation`. The Secret's and Pod's `agents-operator.io/generation` annotation must match, which is how the token rotates and how a dead pod from the previous generation gets replaced.

## Terminal streaming

The runner owns the PTY and the scrollback. The hub is a dumb authenticated pipe. The browser is xterm.js. See [PROTOCOL.md](PROTOCOL.md) for the frames.

What the agent is doing ("running Bash: go test ./...", "needs permission for Edit", "waiting for you", a failed turn) comes from the agent's own hooks rather than from reading the terminal: the runner installs Claude Code hooks that forward every lifecycle event to a loopback endpoint, keeps the current activity, and reports it in `/status`, from which the hub derives the "needs you" badge and the list page's one-line summary (the last reply). Agents without hooks fall back to the old heuristic of a prompt-looking last line plus two seconds of silence.

Closing the tab changes nothing in the pod. Reopening replays the last 2 MiB of raw output from the runner, so the TUI redraws exactly. That only holds at the size the output was drawn for; when a client of a different size attaches (a phone after a desktop, or anyone after the 220×50 default nobody had resized), the browser runs the replay through a headless xterm of the original size and writes what ended up on its screen and scrollback as styled text instead, which wraps cleanly, then sends its own size so the TUI redraws its live frame. The hub never buffers or parses terminal bytes; if the runner connection drops, it closes the browser socket with a reason and the browser reconnects with backoff.

## Credentials

- **API key mode**: values live in the per-user Secret `agents-operator-user-<id>` (managed by `internal/session/credentials.go`). At session creation the reconciler projects the known keys into the per-session Secret as env (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_BASE_URL`, `CLAUDE_CODE_OAUTH_TOKEN`, git identity, `GIT_SSH_KEY`, `GIT_HTTPS_TOKEN`, `GH_TOKEN`).
- **Subscription token**: `claude_oauth_token` is the long-lived token `claude setup-token` prints (the user runs it inside any session and pastes the result on the account page). It reaches Claude Code as `CLAUDE_CODE_OAUTH_TOKEN`, never rotates and is safe to share between any number of sessions, which makes it the recommended way to run a subscription here.
- **First-run prompts**: when a Claude session has something to sign in with (a seeded login, the OAuth token or an API key), the runner answers Claude Code's first-run questions in `~/.claude.json` before the CLI starts: onboarding is marked complete, the API key from the environment is pre-approved, and `/workspace` and every repository directory are marked trusted. For autonomous sessions the consent dialog behind `--dangerously-skip-permissions` is answered in `~/.claude/settings.json`; the user gave that consent by choosing autonomous mode. Existing keys are never changed. Without credentials the CLI's own login flow stays the first screen.
- **GitHub repository picker**: with a `github_token` set, the New Session form can browse the repositories the token can see (`GET /api/v1/me/github/repos`, via `internal/github`, cached per token for five minutes). Entries are ranked by how often the user has started a session with them (the `repo_usage` table, which survives session deletion), then alphabetically; the search box filters by name and description. The hub is the only component that talks to the GitHub API here; it never passes the token to the browser.
- **GitHub**: a per-user `github_token` becomes `GH_TOKEN`/`GITHUB_TOKEN` in the pod. When no SSH key is configured, `git@github.com:` remotes are rewritten to HTTPS through `GIT_CONFIG_*` env so private repositories clone and push with the token alone. The runner image ships the `gh` CLI, so agents can read and comment on PRs, inspect Actions runs and create PRs with the same token; the git credential helper also uses it for `https://github.com` clones and pushes. A fine-grained PAT scoped to the repos in use is the recommended token; a GitHub App with short-lived installation tokens minted by the hub is the natural next step and would slot into the same env vars.
- **Subscription mode**: the user logs in with the CLI inside a session; HOME is on the PVC so it persists. "Save login" asks the runner (over the control channel, hub-initiated only) for the files that make up the CLI's logged-in state (for Claude Code the OAuth tokens in `.claude/.credentials.json` plus `.claude.json`, which holds the onboarding and account state) and stores them as one bundle in the user Secret; later sessions get them seeded into HOME on first boot. Claude Code's OAuth refresh tokens are single use, so a saved pair goes stale as soon as a running session refreshes. The runner therefore reports the credential file's modification time in `/status` (`login_updated_at`, only for writes the CLI made after boot), and the poller re-exports the login from that session whenever it is newer than the account copy. New sessions always start from the newest pair; sessions running side by side still share one pair, which is why the setup token above is preferred.
- The API never returns secret values, only which kinds are set. The hub reads user Secrets only to project them.

## Frontend

Preact + TypeScript + Vite, xterm.js 6 with fit, WebGL (falls back to DOM), web-links, clipboard and unicode11 addons. The clipboard addon implements OSC 52, which is how Claude Code and other TUIs copy the text selected inside them; xterm's own selection (shift+drag while an app has mouse reporting on) copies with the browser's usual shortcut. Phones get a key bar and a selectable text sheet instead. xterm 6 has no touch scrolling of its own (its viewport no longer scrolls natively, unlike xterm 5), so a drag over the terminal is turned into mouse-wheel events, one per row, and kept from the page. Going through the wheel path matters: an app with mouse reporting on, which Claude Code is, receives them as wheel reports and scrolls its own view, exactly as with a mouse, instead of xterm scrolling its buffer past the TUI into stale frames. Taps are left to xterm, whose mousedown handling focuses the textarea. When the on-screen keyboard opens, the page takes the visual viewport's height, so the terminal shrinks, the PTY is resized and the TUI lays itself out for the visible area. No state library: the URL is the state, the list page follows `GET /api/v1/sessions/events` (SSE). Built into `internal/ui/dist` and embedded with `go build -tags ui`; a build without the tag compiles a stub so backend tests never need Node.

## Deliberate deviations from the spec

- **Module path** is `github.com/dseif0x/agents-operator` because that is the repository name; every other name (binary, image, chart, labels, env prefix) is `agents-operator`.
- **SSH key delivery**: the spec mounts the key as a projected volume at `~/.ssh/id_ed25519`. Kubernetes Secret volumes end up group-readable once `fsGroup` applies and OpenSSH refuses such keys, and a read-only mount over `~/.ssh` would break `known_hosts`. The key is therefore passed as env (`GIT_SSH_KEY`) and the runner writes it to `~/.ssh/id_ed25519` with mode 0600 on every boot.
- **Self-updating CLIs**: `/opt/agents` is owned by UID 1000 as the spec asks, but `readOnlyRootFilesystem: true` (a hard requirement) makes it read-only at runtime. CLI updates come from new runner image tags.
- **PID 1**: `tini` is PID 1 and forwards signals; `agent-runner` runs under it and forwards SIGTERM to the agent. This avoids a hand-written zombie reaper.
- **SSE route**: the list feed is `GET /api/v1/sessions/events` (all of the caller's sessions); `GET /api/v1/sessions/{id}/events` returns the per-session event log for the drawer.
- **`shell` agent**: a fourth agent value that runs bash, for debugging pods and for tests.
- **Vite output** goes straight to `internal/ui/dist` instead of `web/dist` plus a copy step; Go's `embed` cannot reach outside the package directory.
