# syntax=docker/dockerfile:1.7
# Runner image: agent-runner (PID 1 via tini) plus the agent CLIs.
# Built for linux/amd64 and linux/arm64. Base images pinned by digest.

FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:a688600ca24f8a4d3ca77f95b0dd40704a9fc787c826660eb7ba0b641b8b175d AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/agent-runner ./cmd/agent-runner

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251
ARG TARGETARCH
ARG VERSION=dev
# Pinned tool versions. Bump via build args or Dependabot-driven PRs.
ARG NODE_VERSION=22.22.2
ARG CLAUDE_CODE_VERSION=latest
ARG OPENCODE_VERSION=latest
ARG CODEX_VERSION=latest

LABEL org.opencontainers.image.title="agents-operator-runner" \
      org.opencontainers.image.description="Session pod image: agent CLIs under a PTY served over WebSocket" \
      org.opencontainers.image.source="https://github.com/dseif0x/agents-operator" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-client ripgrep tmux jq tini xz-utils procps less \
      python3 build-essential locales \
 && sed -i 's/^# *C.UTF-8/C.UTF-8/' /etc/locale.gen && locale-gen \
 && rm -rf /var/lib/apt/lists/*

# Node 22 LTS from nodejs.org, arch-aware.
RUN case "${TARGETARCH}" in amd64) NODE_ARCH=x64 ;; arm64) NODE_ARCH=arm64 ;; *) echo "unsupported arch ${TARGETARCH}" && exit 1 ;; esac \
 && curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz" -o /tmp/node.tar.xz \
 && mkdir -p /opt/node && tar -xJf /tmp/node.tar.xz -C /opt/node --strip-components=1 && rm /tmp/node.tar.xz

# Non-root user; HOME lives on the PVC.
RUN groupadd -g 1000 agent && useradd -m -u 1000 -g 1000 -s /bin/bash -d /workspace/home agent \
 && mkdir -p /workspace /opt/agents && chown -R 1000:1000 /workspace /opt/agents

ENV PATH=/opt/agents/bin:/opt/node/bin:/usr/local/bin:/usr/bin:/bin \
    NPM_CONFIG_PREFIX=/opt/agents \
    LANG=C.UTF-8 LC_ALL=C.UTF-8 \
    HOME=/workspace/home \
    WORKSPACE=/workspace

# Agent CLIs, installed as the agent user under /opt/agents.
USER 1000:1000
RUN npm install -g --no-audit --no-fund \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
      "opencode-ai@${OPENCODE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}" \
 && npm cache clean --force \
 && claude --version && opencode --version && codex --version

COPY --from=build /out/agent-runner /usr/local/bin/agent-runner

WORKDIR /workspace
EXPOSE 7681
# tini reaps zombies and forwards signals; agent-runner forwards SIGTERM to the agent.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/agent-runner"]
