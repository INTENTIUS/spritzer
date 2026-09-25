#!/usr/bin/env bash
# Acceptance for container exec mode on k3d (INTENTIUS/spritzer#22).
#
# Creates a throwaway k3d cluster, builds spritzer's image, imports it and the
# sprite image, deploys spritzer in container mode with its RBAC
# (deploy/k8s/container-mode.yaml), port-forwards it, runs the e2e test, and
# deletes the cluster. KEEP=1 keeps the cluster for poking at.
set -euo pipefail
cd "$(dirname "$0")/.."

cluster="${CLUSTER:-spritzer-e2e-$$}"
ns=spritzer-e2e
tag="spritzer:e2e-$(git rev-parse --short HEAD)"
sprite_image="${SPRITZER_SPRITE_IMAGE:-node:22-bookworm}"
port="${PORT:-4392}"
pf_pid=""

cleanup() {
  if [ -n "$pf_pid" ]; then kill "$pf_pid" 2>/dev/null || true; wait "$pf_pid" 2>/dev/null || true; fi
  if [ "${KEEP:-}" = 1 ]; then
    echo "KEEP=1: cluster $cluster left running (k3d cluster delete $cluster)"
  else
    k3d cluster delete "$cluster" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "--> building $tag"
docker build -q -t "$tag" --build-arg VERSION=e2e .
docker image inspect "$sprite_image" >/dev/null 2>&1 || docker pull -q "$sprite_image"

echo "--> k3d cluster $cluster"
k3d cluster create "$cluster" --wait --no-lb --k3s-arg '--disable=traefik@server:0' >/dev/null
ctx="k3d-$cluster"
k3d image import -c "$cluster" "$tag" "$sprite_image" >/dev/null

echo "--> deploying spritzer (container mode) into $ns"
sed -e "s|__NAMESPACE__|$ns|g" -e "s|__SPRITZER_IMAGE__|$tag|g" -e "s|__SPRITE_IMAGE__|$sprite_image|g" \
  deploy/k8s/container-mode.yaml | kubectl --context "$ctx" apply -f - >/dev/null
kubectl --context "$ctx" -n "$ns" rollout status deploy/spritzer --timeout=180s

kubectl --context "$ctx" -n "$ns" port-forward svc/spritzer "$port:4290" >/dev/null 2>&1 &
pf_pid=$!
for _ in $(seq 1 50); do curl -sf "http://127.0.0.1:$port/_spritzer/health" >/dev/null && break; sleep 0.2; done

echo "--> e2e"
SPRITZER_E2E_URL="http://127.0.0.1:$port" SPRITZER_E2E_RUNTIME=kubernetes \
SPRITZER_E2E_NAMESPACE="$ns" SPRITZER_E2E_CONTEXT="$ctx" \
  go test -tags e2e -count=1 -v ./e2e/
echo "--> spritzer's own log"
kubectl --context "$ctx" -n "$ns" logs deploy/spritzer | tail -5
