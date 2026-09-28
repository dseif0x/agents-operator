#!/usr/bin/env bash
# End-to-end smoke test against a kind cluster set up by hack/kind.sh:
# log in, create a session, wait for it to run, fetch scrollback, stop,
# start, delete, and check the objects go away. Nice to have, not run in CI.
set -euo pipefail

NS=${NS:-agents-operator}
HUB=${HUB:-http://localhost:8080}
JAR=$(mktemp)
trap 'rm -f "$JAR"' EXIT

api() { curl -sS -b "$JAR" -c "$JAR" -H "Content-Type: application/json" -H "X-CSRF-Token: ${CSRF:-}" "$@"; }

CSRF=$(api -X POST "$HUB/api/v1/auth/login" -d '{"username":"admin","password":"admin"}' | jq -r .csrf)
ID=$(api -X POST "$HUB/api/v1/sessions" -d '{"name":"e2e","agent":"shell","repo_url":"https://github.com/octocat/Hello-World.git"}' | jq -r .id)
echo "session $ID"

for _ in $(seq 1 60); do
  state=$(api "$HUB/api/v1/sessions/$ID" | jq -r .state)
  echo "state=$state"
  [ "$state" = running ] && break
  [ "$state" = failed ] && { api "$HUB/api/v1/sessions/$ID/logs"; exit 1; }
  sleep 2
done
[ "$state" = running ]

kubectl -n "$NS" get pod,pvc,secret -l "agents-operator.io/session=$ID"
sleep 3
api "$HUB/api/v1/sessions/$ID/scrollback" | tail -c 300; echo

api -X POST "$HUB/api/v1/sessions/$ID/stop" >/dev/null
for _ in $(seq 1 30); do
  [ "$(api "$HUB/api/v1/sessions/$ID" | jq -r .state)" = stopped ] && break
  sleep 2
done
kubectl -n "$NS" get pvc "agents-operator-$ID" >/dev/null && echo "pvc kept after stop"

api -X POST "$HUB/api/v1/sessions/$ID/start" >/dev/null
for _ in $(seq 1 60); do
  [ "$(api "$HUB/api/v1/sessions/$ID" | jq -r .state)" = running ] && break
  sleep 2
done
echo "restarted on the same pvc"

api -X DELETE "$HUB/api/v1/sessions/$ID" >/dev/null
for _ in $(seq 1 60); do
  if ! kubectl -n "$NS" get pod,pvc,secret -l "agents-operator.io/session=$ID" 2>/dev/null | grep -q agents-operator; then
    echo "all objects gone"; exit 0
  fi
  sleep 2
done
echo "objects still present" >&2
exit 1
