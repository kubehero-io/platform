#!/usr/bin/env bash
# One-command end-to-end KubeHero demo on a local kind cluster.
#
# What runs:
#   1. kind cluster (3 nodes, kube 1.30)
#   2. all 4 product images built + side-loaded
#   3. helm install kubehero with the FULL stack on:
#        postgres + clickhouse + control-plane + collector
#        + operator + dashboard
#   4. synthetic workloads (4 teams, 7 pods)
#   5. a BudgetPolicy + CeilingPolicy sized to trip the burn-rate
#      trigger within ~2-3 minutes of the collector first ingesting
#   6. prints port-forward + dashboard credentials
#
#   ./infra/demo/task-demo.sh           # full setup
#   ./infra/demo/task-demo.sh --clean   # tear down
#
# Requires: docker, kind, helm, kubectl. Takes ~5-7 min cold.

set -euo pipefail

CLUSTER="kubehero-demo"
NS="kubehero-system"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/deploy/helm/kubehero"
TAG="dev"

c_reset="\033[0m"; c_dim="\033[2m"; c_g="\033[32m"; c_b="\033[34m"; c_y="\033[33m"; c_r="\033[31m"
step() { printf "${c_b}▸${c_reset} %s\n" "$*"; }
ok()   { printf "${c_g}✓${c_reset} %s\n" "$*"; }
note() { printf "${c_dim}  %s${c_reset}\n" "$*"; }
warn() { printf "${c_y}!${c_reset} %s\n" "$*"; }
fail() { printf "${c_r}✗${c_reset} %s\n" "$*"; exit 1; }

check_deps() {
  local missing=0
  for bin in docker kind helm kubectl; do
    command -v "$bin" >/dev/null || { warn "missing: $bin"; missing=1; }
  done
  [ $missing -eq 0 ] || fail "install missing deps first"
}

clean() {
  step "tearing down cluster '$CLUSTER'..."
  kind delete cluster --name "$CLUSTER" 2>/dev/null || true
  ok "gone"
}

if [ "${1:-}" = "--clean" ]; then clean; exit 0; fi
check_deps

# ─── 1. cluster ──────────────────────────────────────────────────────────
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  ok "cluster '$CLUSTER' already exists"
else
  step "creating kind cluster '$CLUSTER' (3 nodes)"
  kind create cluster --name "$CLUSTER" --config "$ROOT/infra/demo/kind-config.yaml"
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

# ─── 2. images ───────────────────────────────────────────────────────────
# We tag everything kubehero/<svc>:dev and rely on image.pullPolicy=IfNotPresent
# in the chart to pick up the side-loaded images instead of trying to pull
# from ghcr.
step "building service images"
# name:dockerfile-path tuples — dashboard lives under apps/, the Go services under services/.
images=(
  "collector:services/collector/Dockerfile"
  "control-plane:services/control-plane/Dockerfile"
  "operator:services/operator/Dockerfile"
  "pricing-engine:services/pricing-engine/Dockerfile"
  "dashboard:apps/dashboard/Dockerfile"
)
for entry in "${images[@]}"; do
  name="${entry%%:*}"; dockerfile="${entry#*:}"
  note "  $name"
  docker build -q -f "$ROOT/$dockerfile" -t "kubehero/$name:$TAG" "$ROOT" >/dev/null \
    || fail "$name image build failed"
done
ok "all 5 images built"

step "loading images into kind"
for entry in "${images[@]}"; do
  name="${entry%%:*}"
  kind load docker-image "kubehero/$name:$TAG" --name "$CLUSTER" >/dev/null 2>&1
done
ok "side-loaded into kind"

# ─── 3. chart deps ───────────────────────────────────────────────────────
step "resolving chart dependencies (postgres + clickhouse + dex + trivy)"
helm dep update "$CHART" >/dev/null

# ─── 4. install kubehero (full stack) ────────────────────────────────────
step "installing kubehero (full stack — give it 5-7m)"
helm upgrade --install kubehero "$CHART" \
  --namespace "$NS" --create-namespace \
  --set image.registry= \
  --set image.repository=kubehero \
  --set image.pullPolicy=IfNotPresent \
  --set collector.image.tag=$TAG \
  --set controlPlane.image.tag=$TAG \
  --set operator.image.tag=$TAG \
  --set pricingEngine.image.tag=$TAG \
  --set dashboard.image.tag=$TAG \
  --set cluster.id=demo-cluster \
  --set controlPlane.enabled=true \
  --set operator.enabled=true \
  --set pricingEngine.enabled=true \
  --set dashboard.enabled=true \
  --set postgresql.enabled=true \
  --set postgresql.primary.persistence.enabled=false \
  --set clickhouse.enabled=true \
  --set clickhouse.persistence.enabled=false \
  --wait --timeout 8m

# ─── 5. demo workloads ───────────────────────────────────────────────────
step "applying demo workloads (4 teams, 7 pods)"
kubectl apply -f "$ROOT/infra/demo/demo-workloads.yaml" >/dev/null
kubectl -n ml-inference rollout status deploy/model-server --timeout=120s
kubectl -n retrieval rollout status deploy/vectordb-ingress --timeout=120s
kubectl -n data rollout status deploy/etl-nightly --timeout=120s
kubectl -n edge rollout status deploy/frontend-gateway --timeout=120s

# ─── 6. tripping policy ──────────────────────────────────────────────────
step "applying tripping BudgetPolicy + CeilingPolicy ($5/mo on ml-inference)"
kubectl apply -f "$ROOT/infra/demo/tripping-policy.yaml" >/dev/null
ok "policy installed — burn-rate trigger fires after the next 5m window"

# ─── 7. wait for first ingest batch so the dashboard isn't empty ─────────
step "waiting for first ingest batch (collector → cp → ClickHouse)"
ch_pod=$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=clickhouse \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -n "$ch_pod" ]; then
  for i in {1..30}; do
    rows=$(kubectl -n "$NS" exec "$ch_pod" -- clickhouse-client \
      --user kubehero --password kubehero --database kubehero \
      --query "SELECT count() FROM pod_cost_1s" 2>/dev/null | tr -d '[:space:]' || true)
    if [ -n "$rows" ] && [ "$rows" != "0" ]; then
      ok "ClickHouse has $rows rows"
      break
    fi
    sleep 5
    [ $i -eq 30 ] && warn "no rows after 150s — collector may need more time"
  done
fi

# ─── 8. print access info ────────────────────────────────────────────────
cat <<EOT

${c_g}KUBEHERO DEMO READY${c_reset}

  ${c_b}Dashboard${c_reset}      kubectl -n $NS port-forward svc/kubehero-dashboard 3001:3001
                 then http://localhost:3001/login (any email / 'demo')

  ${c_b}Control plane${c_reset}  kubectl -n $NS port-forward svc/kubehero-control-plane 8080:8080

  ${c_b}Watch policy${c_reset}   kubectl -n $NS describe ceilingpolicy ml-inference-ceiling
                 (the burn rate updates every reconcile loop ~30s)

  ${c_b}Watch audit${c_reset}    curl -fsX POST http://localhost:8080/kubehero.v1.ControlPlaneService/ListAuditLog \\
                   -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \\
                   -d '{"limit":20}' | jq

  ${c_b}Tear down${c_reset}      ./infra/demo/task-demo.sh --clean

EOT
