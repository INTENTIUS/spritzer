#!/usr/bin/env bash
# Acceptance for container exec mode on Docker (INTENTIUS/spritzer#22).
#
# Builds spritzer's image (the agent image sprites get their agent from), runs
# spritzer on the host with SPRITZER_RUNTIME=docker, runs the e2e test, and
# stops spritzer. Sprite containers the test makes are destroyed by the test.
set -euo pipefail
cd "$(dirname "$0")/.."

tag="spritzer:e2e-$(git rev-parse --short HEAD)"
port="${PORT:-4391}"
bin="$(mktemp -d)/spritzer"
pid=""
cleanup() { [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; rm -f "$bin"; }
trap cleanup EXIT

echo "--> building $tag and the host binary"
docker build -q -t "$tag" --build-arg VERSION=e2e .
go build -o "$bin" ./cmd/spritzer

SPRITZER_EXEC=container SPRITZER_RUNTIME=docker SPRITZER_AGENT_IMAGE="$tag" \
SPRITZER_URL_DOMAIN=localhost SPRITZER_ADDR="127.0.0.1:$port" "$bin" &
pid=$!
for _ in $(seq 1 50); do curl -sf "http://127.0.0.1:$port/_spritzer/health" >/dev/null && break; sleep 0.2; done

echo "--> e2e"
SPRITZER_E2E_URL="http://127.0.0.1:$port" SPRITZER_E2E_RUNTIME=docker go test -tags e2e -count=1 -v ./e2e/
