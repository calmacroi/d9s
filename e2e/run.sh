#!/usr/bin/env bash
# Creates throwaway fixtures (all named d9s-e2e*) and runs the UI end-to-end test.
set -euo pipefail
cd "$(dirname "$0")/.."

trap 'docker compose -p d9s-e2e down -v >/dev/null 2>&1 || true' EXIT
docker compose -f e2e/compose.yml up -d web worker >/dev/null 2>&1
docker rm -f d9s-e2e-throwaway >/dev/null 2>&1 || true
docker run -d --name d9s-e2e-throwaway alpine:latest sleep infinity >/dev/null
docker volume create d9s-e2e-tmpvol >/dev/null
docker network inspect d9s-e2e-tmpnet >/dev/null 2>&1 || docker network create d9s-e2e-tmpnet >/dev/null

go test -tags e2e -run TestE2E -count=1 -timeout 8m -v . "$@"
