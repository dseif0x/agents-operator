SHELL := /bin/bash
.DEFAULT_GOAL := build

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
OWNER     ?= dseif0x
HUB_IMAGE ?= ghcr.io/$(OWNER)/agenthub
RUN_IMAGE ?= ghcr.io/$(OWNER)/agenthub-runner
PLATFORMS ?= linux/amd64,linux/arm64
LDFLAGS   := -s -w -X main.version=$(VERSION)

.PHONY: build ui hub runner test test-pg lint fmt image image-hub image-runner chart chart-deps chart-docs dev dev-db dev-db-stop hash-password clean

## build: frontend first, then both binaries with the UI embedded
build: ui hub runner

ui: web/node_modules
	cd web && npm run build

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --no-audit --no-fund

hub:
	CGO_ENABLED=0 go build -tags ui -ldflags "$(LDFLAGS)" -o bin/agenthub ./cmd/agenthub

runner:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/agent-runner ./cmd/agent-runner

## test: Go tests (Postgres tests run when AGENTHUB_TEST_DATABASE_URL is set)
test:
	go test -race -count=1 ./...

## test-pg: Go tests against the compose Postgres
test-pg: dev-db
	AGENTHUB_TEST_DATABASE_URL=postgres://agenthub:agenthub@localhost:5432/agenthub_test?sslmode=disable go test -race -count=1 ./...

## lint: vet, golangci-lint, typecheck, helm lint + unittest
lint:
	go vet ./...
	golangci-lint run ./...
	cd web && npm run typecheck
	helm lint charts/agenthub --strict
	helm unittest charts/agenthub

fmt:
	gofmt -w cmd internal

## image: build both images for the local platform
image: image-hub image-runner

image-hub:
	docker build -f docker/hub.Dockerfile --build-arg VERSION=$(VERSION) -t $(HUB_IMAGE):$(VERSION) .

image-runner:
	docker build -f docker/runner.Dockerfile --build-arg VERSION=$(VERSION) -t $(RUN_IMAGE):$(VERSION) .

## image-multiarch: cross-build both images without pushing (what CI does)
image-multiarch:
	docker buildx build --platform $(PLATFORMS) -f docker/hub.Dockerfile --build-arg VERSION=$(VERSION) -t $(HUB_IMAGE):$(VERSION) .
	docker buildx build --platform $(PLATFORMS) -f docker/runner.Dockerfile --build-arg VERSION=$(VERSION) -t $(RUN_IMAGE):$(VERSION) .

## chart: lint, unit-test and package the chart
chart: chart-deps
	helm lint charts/agenthub --strict
	helm unittest charts/agenthub
	mkdir -p dist && helm package charts/agenthub -d dist

chart-deps:
	helm dependency update charts/agenthub

chart-docs:
	go run github.com/norwoodj/helm-docs/cmd/helm-docs@v1.14.2 --chart-search-root=charts

## dev: run the hub against the current kubeconfig with the compose Postgres
dev: dev-db
	@test -f hack/dev.env || cp hack/dev.env.example hack/dev.env
	set -a && source hack/dev.env && set +a && go run -tags ui ./cmd/agenthub

## dev-db: start the local Postgres from docker compose
dev-db:
	docker compose -f hack/docker-compose.yml up -d --wait

dev-db-stop:
	docker compose -f hack/docker-compose.yml down

## hash-password: print an argon2id hash for auth.adminPasswordHash (reads AGENTHUB_PASSWORD or stdin)
hash-password:
	go run ./cmd/agenthub hash-password

clean:
	rm -rf bin dist internal/ui/dist web/dist charts/agenthub/charts
