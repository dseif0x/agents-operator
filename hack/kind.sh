#!/usr/bin/env bash
# Create a kind cluster, build both images locally and load them, then
# install the chart with a port-forward friendly configuration.
#
#   hack/kind.sh up      # create cluster, build + load images, helm install
#   hack/kind.sh down    # delete the cluster
set -euo pipefail

CLUSTER=${CLUSTER:-agents-operator}
NS=${NS:-agents-operator}
VERSION=${VERSION:-dev}
cd "$(dirname "$0")/.."

case "${1:-up}" in
  up)
    kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER"
    docker build -f docker/hub.Dockerfile --build-arg VERSION="$VERSION" -t "ghcr.io/dseif0x/agents-operator:$VERSION" .
    docker build -f docker/runner.Dockerfile --build-arg VERSION="$VERSION" -t "ghcr.io/dseif0x/agents-operator-runner:$VERSION" .
    kind load docker-image --name "$CLUSTER" "ghcr.io/dseif0x/agents-operator:$VERSION" "ghcr.io/dseif0x/agents-operator-runner:$VERSION"
    helm dependency update charts/agents-operator
    kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
    helm upgrade --install agents-operator charts/agents-operator -n "$NS" \
      --set image.tag="$VERSION" --set image.pullPolicy=Never \
      --set runner.image.tag="$VERSION" --set runner.image.pullPolicy=Never \
      --set runner.storageClass=standard \
      --set publicUrl=http://localhost:8080 \
      --set 'allowedHosts={localhost:8080,127.0.0.1:8080}' \
      --set ingress.enabled=false \
      --set runner.networkPolicy.enabled=false \
      --set auth.adminPasswordHash="$(AGENTS_OPERATOR_PASSWORD=admin go run ./cmd/agents-operator hash-password)" \
      --wait --timeout 10m
    echo
    echo "agents-operator is up. Login admin/admin after:"
    echo "  kubectl -n $NS port-forward svc/agents-operator 8080:80"
    ;;
  down)
    kind delete cluster --name "$CLUSTER"
    ;;
  *)
    echo "usage: $0 up|down" >&2
    exit 2
    ;;
esac
