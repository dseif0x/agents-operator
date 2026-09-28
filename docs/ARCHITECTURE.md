# Architecture

agenthub is a self-hosted mission control for AI coding agents. Every session is a Kubernetes Pod with its own PersistentVolumeClaim, and the agent's real terminal is streamed to the browser. There is no chat UI around an SDK: the CLI's own TUI is the interface.

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
| **hub** (`cmd/agenthub`) | Deployment, 1 replica, distroless | REST API, SSE feed, WebSocket terminal proxy, reconciler, embedded SPA |
| **agent-runner** (`cmd/agent-runner`) | PID 1 in every session pod (under tini) | The PTY, the 2 MiB scrollback ring buffer, the runner WebSocket on `:7681` |
| **Postgres** | Bitnami subchart or external | users, sessions, session_events, user_credentials |

The browser only talks to the hub. The hub reaches runners by pod IP on the cluster network and is the only component that calls the Kubernetes API. Session pods have no Kubernetes identity at all (`automountServiceAccountToken: false`).

## Packages

| Package | Responsibility |
| --- | --- |
| `internal/config` | Typed config from `AGENTHUB_*` env; fails fast |
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

A session is one row, one PVC, one Secret and zero or one Pod, all named `agenthub-<id>` and labelled `agenthub.io/session=<id>`. The PVC is the identity that survives; the Pod is disposable.

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

Starting a stopped or failed session bumps `generation`. The Secret's and Pod's `agenthub.io/generation` annotation must match, which is how the token rotates and how a dead pod from the previous generation gets replaced.

## Terminal streaming

The runner owns the PTY and the scrollback. The hub is a dumb authenticated pipe. The browser is xterm.js. See [PROTOCOL.md](PROTOCOL.md) for the frames.

Closing the tab changes nothing in the pod. Reopening replays the last 2 MiB of raw output from the runner, so the TUI redraws exactly. The hub never buffers or parses terminal bytes; if the runner connection drops, it closes the browser socket with a reason and the browser reconnects with backoff.

## Credentials

- **API key mode**: values live in the per-user Secret `agenthub-user-<id>` (managed by `internal/session/credentials.go`). At session creation the reconciler projects the known keys into the per-session Secret as env (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_BASE_URL`, git identity, `GIT_SSH_KEY`, `GIT_HTTPS_TOKEN`).
- **Subscription mode**: the user logs in with the CLI inside a session; HOME is on the PVC so it persists. "Save login" asks the runner (over the control channel, hub-initiated only) for the CLI's credential file and stores it in the user Secret; later sessions get it seeded into HOME on first boot.
- The API never returns secret values, only which kinds are set. The hub reads user Secrets only to project them.

## Frontend

Preact + TypeScript + Vite, xterm.js 6 with fit, WebGL (falls back to DOM), web-links and unicode11 addons. No state library: the URL is the state, the list page follows `GET /api/v1/sessions/events` (SSE). Built into `internal/ui/dist` and embedded with `go build -tags ui`; a build without the tag compiles a stub so backend tests never need Node.

## Deliberate deviations from the spec

- **Module path** is `github.com/dseif0x/agents-operator` because that is the repository name; every other name (binary, image, chart, labels, env prefix) is `agenthub`.
- **SSH key delivery**: the spec mounts the key as a projected volume at `~/.ssh/id_ed25519`. Kubernetes Secret volumes end up group-readable once `fsGroup` applies and OpenSSH refuses such keys, and a read-only mount over `~/.ssh` would break `known_hosts`. The key is therefore passed as env (`GIT_SSH_KEY`) and the runner writes it to `~/.ssh/id_ed25519` with mode 0600 on every boot.
- **Self-updating CLIs**: `/opt/agents` is owned by UID 1000 as the spec asks, but `readOnlyRootFilesystem: true` (a hard requirement) makes it read-only at runtime. CLI updates come from new runner image tags.
- **PID 1**: `tini` is PID 1 and forwards signals; `agent-runner` runs under it and forwards SIGTERM to the agent. This avoids a hand-written zombie reaper.
- **SSE route**: the list feed is `GET /api/v1/sessions/events` (all of the caller's sessions); `GET /api/v1/sessions/{id}/events` returns the per-session event log for the drawer.
- **`shell` agent**: a fourth agent value that runs bash, for debugging pods and for tests.
- **Vite output** goes straight to `internal/ui/dist` instead of `web/dist` plus a copy step; Go's `embed` cannot reach outside the package directory.
