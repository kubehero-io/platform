#!/usr/bin/env bash
# End-to-end KubeHero chargeback demo on a local kind cluster.
#
#   ./infra/demo/kind-demo.sh           # full setup
#   ./infra/demo/kind-demo.sh --clean   # tear down
#
# Requires: docker, kind, helm, kubectl.

set -euo pipefail

CLUSTER="kubehero-demo"
NS="kubehero-system"
MON_NS="monitoring"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/deploy/helm/kubehero"
IMG="kubehero/collector:dev"

c_reset="\033[0m"; c_dim="\033[2m"; c_g="\033[32m"; c_b="\033[34m"; c_y="\033[33m"
step() { printf "${c_b}▸${c_reset} %s\n" "$*"; }
ok()   { printf "${c_g}✓${c_reset} %s\n" "$*"; }
note() { printf "${c_dim}  %s${c_reset}\n" "$*"; }
warn() { printf "${c_y}!${c_reset} %s\n" "$*"; }

check_deps() {
  local missing=0
  for bin in docker kind helm kubectl; do
    if ! command -v "$bin" >/dev/null; then
      warn "missing: $bin"
      missing=1
    fi
  done
  [ $missing -eq 0 ] || { echo "install missing deps first"; exit 1; }
}

clean() {
  step "tearing down cluster '$CLUSTER'..."
  kind delete cluster --name "$CLUSTER" 2>/dev/null || true
  ok "gone"
}

if [ "${1:-}" = "--clean" ]; then
  clean
  exit 0
fi

check_deps

# ─── 1. cluster ──────────────────────────────────────────────────────────
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  ok "cluster '$CLUSTER' already exists"
else
  step "creating kind cluster '$CLUSTER' (3 nodes)"
  kind create cluster --name "$CLUSTER" --config "$ROOT/infra/demo/kind-config.yaml"
fi
kubectl config use-context "kind-$CLUSTER" >/dev/null

# ─── 2. collector image ──────────────────────────────────────────────────
step "building collector image ($IMG)"
docker build -f "$ROOT/services/collector/Dockerfile" -t "$IMG" "$ROOT"
step "loading image into kind"
kind load docker-image "$IMG" --name "$CLUSTER"

# ─── 3. kube-prometheus-stack ────────────────────────────────────────────
step "installing kube-prometheus-stack into ns/$MON_NS"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update >/dev/null
helm upgrade --install kps prometheus-community/kube-prometheus-stack \
  --namespace "$MON_NS" --create-namespace \
  --set grafana.service.type=NodePort \
  --set grafana.service.nodePort=30080 \
  --set grafana.adminPassword=kubehero \
  --set grafana.defaultDashboardsEnabled=false \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
  --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
  --set prometheus.prometheusSpec.ruleSelectorNilUsesHelmValues=false \
  --wait --timeout 7m

# ─── 4. kubehero chart ───────────────────────────────────────────────────
step "installing kubehero chart (collector-only profile)"
helm upgrade --install kubehero "$CHART" \
  --namespace "$NS" --create-namespace \
  --set image.registry= \
  --set image.repository=kubehero \
  --set image.pullPolicy=IfNotPresent \
  --set collector.image.tag=dev \
  --set controlPlane.enabled=false \
  --set operator.enabled=false \
  --set pricingEngine.enabled=false \
  --set dashboard.enabled=false \
  --set prometheus.release=kps \
  --wait --timeout 3m

# ─── 5. demo workloads ───────────────────────────────────────────────────
step "applying demo workloads (4 teams, 7 pods)"
kubectl apply -f "$ROOT/infra/demo/demo-workloads.yaml"
kubectl -n ml-inference rollout status deploy/model-server
kubectl -n retrieval rollout status deploy/vectordb-ingress
kubectl -n data rollout status deploy/etl-nightly
kubectl -n edge rollout status deploy/frontend-gateway

# ─── 6. sanity check the pipeline ────────────────────────────────────────
step "checking collector is emitting chargeback metrics"
sleep 5
pod=$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=collector -o jsonpath='{.items[0].metadata.name}')
count=$(kubectl -n "$NS" exec "$pod" -- \
  sh -c "wget -qO- http://localhost:8081/metrics | grep -c kubehero_pod_cost_usd_per_second" 2>/dev/null || true)
count="${count:-0}"
ok "collector emitting ${count} kubehero_pod_cost_usd_per_second series"

# ─── 7. print access info ────────────────────────────────────────────────
cat <<EOT

${c_g}DEMO READY${c_reset}

  Grafana    http://localhost:30080
  login      admin / kubehero
  dashboards Dashboards → "KubeHero" folder:
               · KubeHero — Chargeback by team
               · KubeHero — Fleet
               · KubeHero — GPU panel

  PromQL     port-forward prometheus for raw queries:
               kubectl -n ${MON_NS} port-forward svc/kps-kube-prometheus-stack-prometheus 9090:9090
               then http://localhost:9090 — try:
                 kubehero:team_cost_usd:rate1h
                 kubehero:team_gpu_idle_cost_usd:rate1h

  Tear down  ./infra/demo/kind-demo.sh --clean

EOT
