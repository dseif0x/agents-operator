# agents-operator

Self-hosted mission control for AI coding agents on Kubernetes. Every session is its own pod with its own PersistentVolumeClaim, and the agent's real terminal (Claude Code, OpenCode, Codex) is streamed to the browser with full colour and keyboard. Close the tab, nothing dies; reopen it from your phone and the TUI is exactly where you left it.

One Go binary serves the API, the WebSocket terminal proxy, the Kubernetes orchestration and the embedded web UI. It ships as a Helm chart with Postgres as a chart dependency and runs on a plain k3s cluster with NFS storage, Traefik and cert-manager.

**This is not** a chat UI around an agent SDK, a multi-tenant SaaS, a sandbox service with object storage, or a Docker/tmux runtime. Sessions are pods. State lives on a PVC and in Postgres and nowhere else.

## How it works

```
browser ──▶ hub ──▶ session pod (agent-runner + agent CLI, /workspace on a PVC)
             │
             └──▶ Postgres (session registry)
```

- The **hub** (`cmd/agents-operator`) is a single-replica Deployment. It creates a PVC, a Secret and a Pod per session, proxies the terminal WebSocket by pod IP, and serves the SPA.
- **agent-runner** (`cmd/agent-runner`) is the entrypoint of the session pod. It clones the session's repositories on first boot (one or many, side by side under `/workspace`), runs the agent under a PTY, keeps 2 MiB of scrollback and serves it over a WebSocket. Reconnects replay the buffer, so the TUI redraws correctly.
- Stop deletes the pod and keeps the PVC. Start recreates the pod on the same PVC. Delete removes everything.

Details: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md), [docs/PROTOCOL.md](docs/PROTOCOL.md), [docs/OPERATIONS.md](docs/OPERATIONS.md), chart values in [charts/agents-operator/README.md](charts/agents-operator/README.md).

## Install

```sh
helm repo add agents-operator https://dseif0x.github.io/agents-operator/
helm install agents-operator agents-operator/agents-operator -n agents-operator --create-namespace \
  --set publicUrl=https://agents-operator.example.com \
  --set ingress.host=agents-operator.example.com
```

The NOTES print how to read the generated admin password. Set your model credentials on the account page: an API key, or for a Claude subscription the token printed by `claude setup-token` (run it inside any session; the token never expires between sessions). Logging in with the CLI inside a session and pressing *Save login* works too, and the hub keeps that copy current as the CLI rotates its tokens. Add a GitHub token there too and agents get `gh` plus HTTPS access to github.com for PRs, checks and Actions runs.

## Security model

Agents run with permission checks skipped, on real repos, with real credentials, so the pod boundary is the only boundary:

- non-root (UID 1000), read-only root filesystem, all capabilities dropped, seccomp RuntimeDefault, no service account token, no host network/paths/privileged. There is no values flag to relax any of this.
- default-deny NetworkPolicy: DNS, 443 and 22 outward (minus cluster ranges), inbound only from the hub.
- per-session random runner tokens rotated on every start; per-user credentials in Kubernetes Secrets, never echoed by the API.
- hub: distroless, cookie sessions (`HttpOnly; Secure; SameSite=Lax`), CSRF header on writes, Origin and Host allowlists, login rate limiting, argon2id.
- namespace-scoped Role with exactly the verbs needed, checked in CI with `kubectl auth can-i --list`.

**Out of scope for v1**: multi-user isolation beyond "you cannot see other users' sessions" (all pods share a namespace), audit log export, OIDC (the `auth.Authenticator` interface is the drop-in point).

## Development

```sh
make build        # frontend, then both binaries with the UI embedded
make test         # Go tests (set AGENTS_OPERATOR_TEST_DATABASE_URL for the Postgres store tests)
make lint         # go vet, golangci-lint, tsc, helm lint, helm unittest
make dev          # hub against your kubeconfig with a compose Postgres
hack/kind.sh up   # full stack on kind
```

`go build ./...` without `-tags ui` compiles a stub UI, so backend work never needs Node. CI runs the Go tests against a Postgres service container, builds the SPA and the embedded binary, lints and unit-tests the chart, validates it on kind, and cross-builds both images for amd64 and arm64.

Releases are tags: `v1.2.3` produces both images (signed, with SBOM), chart `1.2.3` and an updated `index.yaml` on GitHub Pages. See [docs/OPERATIONS.md](docs/OPERATIONS.md#releases).

## Layout

```
cmd/agents-operator          hub binary            internal/reconcile   pod/PVC/secret specs + converge loop
cmd/agent-runner      runner binary         internal/session     business logic, poller, SSE, credentials
internal/api          HTTP + WebSocket      internal/term        terminal proxy
internal/auth         login, cookies, CSRF  internal/ui          embedded SPA
internal/store        Postgres + migrations web/                 Preact + xterm.js app
charts/agents-operator       Helm chart            docker/              hub and runner Dockerfiles
.github/workflows     ci, release-images, release-chart
```

## License

MIT
