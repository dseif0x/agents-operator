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

- The **hub** (`cmd/agents-operator`) is a single-replica Deployment, upgraded with a rolling surge and a leader Lease so the UI and terminals stay up. It creates a PVC, a Secret and a Pod per session, proxies the terminal WebSocket by pod IP, and serves the SPA.
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

The NOTES print how to read the generated admin password. Add your accounts on the account page, as many as you like: Anthropic and OpenAI API keys, Claude subscription tokens printed by `claude setup-token` (run it inside any session; the token never expires between sessions), OpenAI-compatible endpoints such as LiteLLM, and Unjailed keys. Claude Code and Codex sessions sign in with the account picked for the session or your default; OpenCode gets every key and endpoint as a provider under its name. Git accounts go there as well, one HTTPS token and one SSH key per host; with a token for github.com agents get `gh` plus HTTPS access to github.com for PRs, checks and Actions runs.

## Security model

Agents run with permission checks skipped, on real repos, with real credentials, so the pod boundary is the only boundary:

- non-root (UID 1000), read-only root filesystem, all capabilities dropped, seccomp RuntimeDefault, no service account token unless a session opts into Kubernetes access (read-only, or read-only plus `edit` in namespaces its owner lists; or a sandbox: a vcluster of its own and only its kubeconfig), no host network/paths/privileged. There is no values flag to relax any of this.
- default-deny NetworkPolicy: cluster DNS, and 443 and 22 outward to public addresses only (no private or link-local ranges, so neither the cluster, the nodes nor your LAN), inbound only from the hub. A session can open specific addresses on your network for its agent (local network access), within ranges the chart allows and never the cluster's own.
- per-session random runner tokens rotated on every start; per-user credentials in Kubernetes Secrets, never echoed by the API. A session gets only the AI accounts its agent uses (Claude Code and Codex exactly one); git credentials go to every agent unless the session turns them off, in which case the runner only clones with them and the agent can commit but not push.
- the runner token, SSH key and saved-login seeds never sit in the agent's reach: they are moved out of PID 1's environment before it starts, and the runner, which holds them, is non-dumpable.
- hub: distroless, cookie sessions (`HttpOnly; Secure; SameSite=Lax`), CSRF header on writes, Origin and Host allowlists, login rate limiting, argon2id.
- namespace-scoped Role with exactly the verbs needed; cluster-wide rights only when namespace write access for sessions is turned on, and only the ones that needs.

**Out of scope for v1**: multi-user isolation beyond "you cannot see other users' sessions" (all pods share a namespace), audit log export, OIDC (the `auth.Authenticator` interface is the drop-in point).

## Development

```sh
make build        # frontend, then both binaries with the UI embedded
make test         # Go tests (set AGENTS_OPERATOR_TEST_DATABASE_URL for the Postgres store tests)
make lint         # go vet, golangci-lint, tsc, helm lint, helm unittest
make dev          # hub against your kubeconfig with a compose Postgres
hack/kind.sh up   # full stack on kind
```

`go build ./...` without `-tags ui` compiles a stub UI, so backend work never needs Node. There is no CI on pull requests: run `make lint test` (and `hack/kind.sh up` for the full stack) before merging.

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
