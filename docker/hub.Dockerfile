# syntax=docker/dockerfile:1.7
# Hub image: Node builds the SPA, Go embeds it, distroless runs it.
# Base images are pinned by digest; Dependabot keeps the digests fresh.

FROM --platform=$BUILDPLATFORM node:26-bookworm-slim@sha256:662933cf47f013bc8e4beb31a6116448427a82057ba7c42c97e4c5ba766504c2 AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

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
COPY --from=web /src/internal/ui/dist internal/ui/dist
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -tags ui -ldflags="-s -w -X main.version=${VERSION}" -o /out/agenthub ./cmd/agenthub

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev
LABEL org.opencontainers.image.title="agenthub" \
      org.opencontainers.image.description="Mission control for AI coding agents on Kubernetes" \
      org.opencontainers.image.source="https://github.com/dseif0x/agents-operator" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/agenthub /agenthub
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/agenthub"]
