# Architecture

agents-operator is a self-hosted mission control for AI coding agents. Every session is a Kubernetes Pod with its own PersistentVolumeClaim, and the agent's real terminal is streamed to the browser. There is no chat UI around an SDK: the CLI's own TUI is the interface.

## Components

```
 browser ──HTTPS/WSS──▶ hub (Go, 1 replica) ──ws://podIP:7681──▶ session pod
                          │  REST API, SSE                       │ agent-runner (PTY owner)
                          │  WebSocket proxy                     │ claude / opencode / codex
                          │  reconciler (Pods, PVCs, Secrets)    │ /workspace  ← PVC
                          ▼                                      │ RUNNER_TOKEN ← Secret
                        Postgres (session registry)
```

| Piece | Runs as | Owns |
| --- | --- | --- |
| **hub** (`cmd/agents-operator`) | Deployment, 1 replica, distroless | REST API, SSE feed, WebSocket terminal proxy, reconciler, embedded SPA |
| **agent-runner** (`cmd/agent-runner`) | PID 1 in every session pod (under tini) | The PTY, the 2 MiB scrollback ring buffer, the runner WebSocket on `:7681` |
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

Closing the tab changes nothing in the pod. Reopening replays the last 2 MiB of raw output from the runner, so the TUI redraws exactly. The hub never buffers or parses terminal bytes; if the runner connection drops, it closes the browser socket with a reason and the browser reconnects with backoff.

## Credentials

- **API key mode**: values live in the per-user Secret `agents-operator-user-<id>` (managed by `internal/session/credentials.go`). At session creation the reconciler projects the known keys into the per-session Secret as env (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_BASE_URL`, git identity, `GIT_SSH_KEY`, `GIT_HTTPS_TOKEN`, `GH_TOKEN`).
- **GitHub repository picker**: with a `github_token` set, the New Session form can browse the repositories the token can see (`GET /api/v1/me/github/repos`, via `internal/github`, cached per token for five minutes). Entries are ranked by how often the user has started a session with them (the `repo_usage` table, which survives session deletion), then alphabetically; the search box filters by name and description. The hub is the only component that talks to the GitHub API here; it never passes the token to the browser.
- **GitHub**: a per-user `github_token` becomes `GH_TOKEN`/`GITHUB_TOKEN` in the pod. When no SSH key is configured, `git@github.com:` remotes are rewritten to HTTPS through `GIT_CONFIG_*` env so private repositories clone and push with the token alone. The runner image ships the `gh` CLI, so agents can read and comment on PRs, inspect Actions runs and create PRs with the same token; the git credential helper also uses it for `https://github.com` clones and pushes. A fine-grained PAT scoped to the repos in use is the recommended token; a GitHub App with short-lived installation tokens minted by the hub is the natural next step and would slot into the same env vars.
- **Subscription mode**: the user logs in with the CLI inside a session; HOME is on the PVC so it persists. "Save login" asks the runner (over the control channel, hub-initiated only) for the CLI's credential file and stores it in the user Secret; later sessions get it seeded into HOME on first boot.
- The API never returns secret values, only which kinds are set. The hub reads user Secrets only to project them.

## Frontend

Preact + TypeScript + Vite, xterm.js 6 with fit, WebGL (falls back to DOM), web-links and unicode11 addons. No state library: the URL is the state, the list page follows `GET /api/v1/sessions/events` (SSE). Built into `internal/ui/dist` and embedded with `go build -tags ui`; a build without the tag compiles a stub so backend tests never need Node.

## Deliberate deviations from the spec

- **Module path** is `github.com/dseif0x/agents-operator` because that is the repository name; every other name (binary, image, chart, labels, env prefix) is `agents-operator`.
- **SSH key delivery**: the spec mounts the key as a projected volume at `~/.ssh/id_ed25519`. Kubernetes Secret volumes end up group-readable once `fsGroup` applies and OpenSSH refuses such keys, and a read-only mount over `~/.ssh` would break `known_hosts`. The key is therefore passed as env (`GIT_SSH_KEY`) and the runner writes it to `~/.ssh/id_ed25519` with mode 0600 on every boot.
- **Self-updating CLIs**: `/opt/agents` is owned by UID 1000 as the spec asks, but `readOnlyRootFilesystem: true` (a hard requirement) makes it read-only at runtime. CLI updates come from new runner image tags.
- **PID 1**: `tini` is PID 1 and forwards signals; `agent-runner` runs under it and forwards SIGTERM to the agent. This avoids a hand-written zombie reaper.
- **SSE route**: the list feed is `GET /api/v1/sessions/events` (all of the caller's sessions); `GET /api/v1/sessions/{id}/events` returns the per-session event log for the drawer.
- **`shell` agent**: a fourth agent value that runs bash, for debugging pods and for tests.
- **Vite output** goes straight to `internal/ui/dist` instead of `web/dist` plus a copy step; Go's `embed` cannot reach outside the package directory.
