#!/usr/bin/env bash
# One-command KubeHero demo on a local kind cluster: the real chart
# (embedded Postgres + ClickHouse, auth on), a small live "shop"
# workload, every signal verified end to end — then a BudgetPolicy +
# CeilingPolicy sized to trip, so you can watch the guarded escalation.
#
#   ./infra/demo/task-demo.sh           # full setup (~6-10 min cold)
#   ./infra/demo/task-demo.sh --clean   # tear down
#
# Requires: docker, kind, helm, kubectl, curl, jq.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
export CLUSTER="${CLUSTER:-kubehero-demo}"
NS="kubehero-system"

if [ "${1:-}" = "--clean" ]; then
  kind delete cluster --name "$CLUSTER"; exit 0
fi

KEEP=1 "$ROOT/infra/demo/e2e-kind.sh"

kubectl apply -f "$ROOT/infra/demo/demo-workloads.yaml" >/dev/null
kubectl apply -f "$ROOT/infra/demo/tripping-policy.yaml" >/dev/null
TOKEN=$(kubectl -n "$NS" get secret kubehero-control-plane -o jsonpath='{.data.admin-token}' | base64 -d)

cat <<EOT

KUBEHERO DEMO READY

  Dashboard      kubectl -n $NS port-forward svc/kubehero-dashboard 3001:3001
                 http://localhost:3001 — sign in with the admin token:
                 $TOKEN

  CLI            kubectl -n $NS port-forward svc/kubehero-control-plane 8080:8080
                 export KUBEHERO_ENDPOINT=http://localhost:8080 KUBEHERO_TOKEN=$TOKEN
                 kubehero cost allocation --aggregate namespace --window 1h
                 kubehero logs '{namespace="shop"} |= "failed"' --since 15m
                 kubehero ask "which service is burning the most CPU?"

  Watch policy   kubectl -n $NS describe ceilingpolicy ml-inference-ceiling

  Tear down      ./infra/demo/task-demo.sh --clean
EOT
